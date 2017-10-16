//go:generate easyjson main.go
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net/http"
	_ "net/http/pprof"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/dchest/safefile"
	"github.com/gogits/chardet"
	"github.com/mailru/easyjson"
	blake2b "github.com/minio/blake2b-simd"
	"github.com/veqryn/go-email/email"
	"golang.org/x/text/encoding/htmlindex"
	elastic "gopkg.in/olivere/elastic.v5"
)

var attachdir = flag.String("attachdir", "files", "path to the attachments directory")
var elasticUrl = flag.String("elastic", "http://127.0.0.1:9200", "URL of the ElasticSearch server")
var elasticIndex = flag.String("index", "mail", "name of the ElasticSearch index")
var doInit = flag.Bool("init", false, "whether to initialize the index instead of indexing mail")
var profileaddr = flag.String("profileaddr", "", "address for the performance profiler server to listen on")
var htmlDetector = chardet.NewHtmlDetector()
var textDetector = chardet.NewTextDetector()
var wordDecoder = new(mime.WordDecoder)
var addrNumRegex = regexp.MustCompile(`>\s*\(\d+\)`) // For fixing stuff like: "Some Name" <mail@example.com> (19290960)

const indexSettings string = `{
	"mappings": {
		"msg": {
			"properties": {
				"h": {
					"properties": {
						"Date": { "type": "date", "format": "EEE, dd MMM yyyy HH:mm:ss Z" },
						"Subject": { "type": "text" },
						"Message-Id": { "type": "keyword" },
						"From": { "type": "keyword" },
						"To": { "type": "keyword" },
						"Cc": { "type": "keyword" },
						"Bcc": { "type": "keyword" },
						"Return-Path": { "type": "keyword" },
						"Delivered-To": { "type": "keyword" },
						"Dkim-Signature": { "type": "text", "index": false },
						"X-Google-Dkim-Signature": { "type": "text", "index": false }
					}
				},
				"a": { "type": "keyword" },
				"t": { "type": "text" }
			}
		}
	}
}`

//easyjson:json
type JMessage struct {
	Id         string       `json:"_id,omitempty"`
	Header     email.Header `json:"h,omitempty"`
	Preamble   []byte       `json:"pre,omitempty"`
	Epilogue   []byte       `json:"epi,omitempty"`
	Parts      []*JMessage  `json:"p,omitempty"`
	SubMessage *JMessage    `json:"sub,omitempty"`
	TextBody   string       `json:"t,omitempty"`
	Attachment string       `json:"a,omitempty"`
}

func normalizeAddrs(vals []string) []string {
	result := make([]string, 0)
	for _, val := range vals {
		addrs, err := mail.ParseAddressList(addrNumRegex.ReplaceAllString(val, ">"))
		if err != nil {
			log.Printf("Could not parse address list, skipping normalization: %s", val)
			result = append(result, val)
		} else {
			for _, addr := range addrs {
				result = append(result, addr.Address)
			}
		}
	}
	return result
}

func decodeCharset(charset string, body []byte, description string, ishtml bool) ([]byte, string, error) {
	var err error
	if charset == "" {
		var detenc *chardet.Result
		if ishtml {
			detenc, err = htmlDetector.DetectBest(body)
		} else {
			detenc, err = textDetector.DetectBest(body)
		}
		if err != nil {
			charset = detenc.Charset
			log.Printf("No charset in %s, detected %s (lang %s, confidence %d%%)",
				description, detenc.Charset, detenc.Language, detenc.Confidence)
		} else {
			charset = "utf-8"
			log.Printf("No charset in %s, detected nothing, assuming UTF-8", description)
		}
	}
	enc, err := htmlindex.Get(charset)
	if err != nil || enc == nil {
		return nil, charset, err
	}
	decoded, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		return nil, charset, err
	}
	return decoded, charset, nil
}

func decodeReader(charset string, input io.Reader) (io.Reader, error) {
	body, err := ioutil.ReadAll(input)
	if err != nil {
		return nil, err
	}
	decoded, _, err := decodeCharset(charset, body, fmt.Sprintf("header '%s'", body), false)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(decoded), nil
}

func jsonifyMsg(msg email.Message) JMessage {
	result := JMessage{
		Id:         msg.Header.Get("Message-Id"),
		Header:     msg.Header,
		Preamble:   msg.Preamble,
		Epilogue:   msg.Epilogue,
		Parts:      []*JMessage{},
		SubMessage: nil,
		TextBody:   "",
		Attachment: "",
	}
	delete(result.Header, "Message-Id")
	for k, vs := range result.Header {
		for i, v := range vs {
			dec, err := wordDecoder.DecodeHeader(v)
			if err != nil {
				log.Printf("Could not decode header %s [%d] '%s': %v", k, i, v, err)
				continue
			}
			result.Header[k][i] = dec
		}
	}
	result.Header["From"] = normalizeAddrs(result.Header["From"])
	result.Header["To"] = normalizeAddrs(result.Header["To"])
	result.Header["Cc"] = normalizeAddrs(result.Header["Cc"])
	result.Header["Bcc"] = normalizeAddrs(result.Header["Bcc"])
	result.Header["Return-Path"] = normalizeAddrs(result.Header["Return-Path"])
	result.Header["Delivered-To"] = normalizeAddrs(result.Header["Delivered-To"])
	if msg.SubMessage != nil {
		submsg := jsonifyMsg(*msg.SubMessage)
		result.SubMessage = &submsg
	}
	for _, part := range msg.Parts {
		if part != nil {
			partmsg := jsonifyMsg(*part)
			result.Parts = append(result.Parts, &partmsg)
		}
	}
	ctype := result.Header.Get("Content-Type")
	if strings.HasPrefix(ctype, "text") {
		mediatype, params, err := mime.ParseMediaType(ctype)
		if err != nil {
			if strings.Contains(ctype, "html") {
				mediatype = "text/html"
			} else {
				mediatype = "text/plain"
			}
			params = make(map[string]string)
			log.Printf("Unreadable Content-Type: %s: %v, assuming %s", ctype, err, mediatype)
		}
		decoded, charset, err := decodeCharset(
			params["charset"],
			msg.Body,
			fmt.Sprintf("Content-Type: %s", ctype),
			strings.Contains(mediatype, "html"))
		if err != nil {
			log.Printf("Could not decode body (%s): %v", charset, err)
			goto file
		}
		result.TextBody = string(decoded)
		return result
	}
file:
	hash := blake2b.Sum256(msg.Body)
	path := filepath.Join(*attachdir, hex.EncodeToString(hash[:]))
	result.Attachment = path
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		log.Printf("File already exists: %s", path)
		return result
	}
	f, err := safefile.Create(path, 0444)
	if err != nil {
		log.Printf("Could not open file %s: %v", path, err)
	}
	defer f.Close()
	_, err = f.Write(msg.Body)
	if err != nil {
		log.Printf("Could not write to file %s: %v", path, err)
	}
	err = f.Commit()
	if err != nil {
		log.Printf("Could not commit file %s: %v", path, err)
	}
	return result
}

func process(msgtext io.Reader) (*JMessage, error) {
	log.Printf("Parsing...")
	msg, err := email.ParseMessage(msgtext)
	if err != nil {
		return nil, err
	}
	jmsg := jsonifyMsg(*msg)
	return &jmsg, nil
}

func main() {
	wordDecoder.CharsetReader = decodeReader
	flag.Parse()
	if *profileaddr != "" {
		go func() {
			log.Println(http.ListenAndServe(*profileaddr, nil))
		}()
	}
	ctx := context.Background()
	client, err := elastic.NewClient(
		elastic.SetURL(*elasticUrl),
	)
	if err != nil {
		log.Fatalf("Could not create ElasticSearch client: %v", err)
	}
	if *doInit {
		res, err := client.CreateIndex(*elasticIndex).BodyString(indexSettings).Do(ctx)
		if err != nil {
			log.Fatalf("Could not initialize index: %v", err)
		} else {
			log.Printf("Result: %v", res)
		}
	} else if len(flag.Args()) == 0 || flag.Arg(0) == "-" {
		jmsg, err := process(bufio.NewReader(os.Stdin))
		if err != nil {
			log.Fatalf("Error parsing envelope: %v ", err)
		}
		j, err := easyjson.Marshal(*jmsg)
		if err != nil {
			log.Fatalf("Error serializing JSON: %v", err)
		}
		_, err = client.Index().Index(*elasticIndex).Type("msg").Id(jmsg.Id).BodyString(string(j)).Do(ctx)
		if err != nil {
			log.Fatalf("Error indexing: %v", err)
		}
	} else {
		proc, err := client.BulkProcessor().Name("mail2elasticsearch").Do(ctx)
		if err != nil {
			log.Fatalf("Could not start bulk processor: %v", err)
		}
		defer proc.Close()
		var wg sync.WaitGroup
		tasks := make(chan string)
		for i := 0; i < runtime.NumCPU(); i++ {
			go func() {
				for {
					filename := <-tasks
					file, err := os.Open(filename)
					if err != nil {
						log.Printf("Error opening file ", err)
						continue
					}
					jmsg, err := process(bufio.NewReader(file))
					if err != nil {
						log.Printf("Error parsing envelope: %v ", err)
						continue
					}
					j, err := easyjson.Marshal(*jmsg)
					if err != nil {
						log.Printf("Error serializing JSON: %v", err)
						continue
					}
					proc.Add(elastic.NewBulkIndexRequest().Index(*elasticIndex).Type("msg").Id(jmsg.Id).Doc(string(j)))
					wg.Done()
				}
			}()
		}
		for _, filename := range flag.Args() {
			f, err := os.Stat(filename)
			if err != nil {
				log.Fatalf("Could not stat file: %v", err)
			}
			if f.Mode().IsDir() {
				err = filepath.Walk(filename, func(path string, _ os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					f, err := os.Stat(path)
					if err != nil {
						log.Fatalf("Could not stat file: %v", err)
					}
					if f.Mode().IsRegular() {
						wg.Add(1)
						tasks <- path
					} else {
						log.Printf("Not a file: %s", path)
					}
					return nil
				})
				if err != nil {
					log.Fatalf("Could not walk file: %v", err)
				}
			} else {
				wg.Add(1)
				tasks <- filename
			}
		}
		wg.Wait()
	}
}
