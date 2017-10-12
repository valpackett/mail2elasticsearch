//go:generate easyjson main.go
package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net/mail"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/mailru/easyjson"
	blake2b "github.com/minio/blake2b-simd"
	"github.com/veqryn/go-email/email"
	"golang.org/x/text/encoding/ianaindex"
	elastic "gopkg.in/olivere/elastic.v5"
)

var attachdir = flag.String("attachdir", "files", "path to the attachments directory")
var elasticUrl = flag.String("elastic", "http://127.0.0.1:9200", "URL of the ElasticSearch server")
var elasticIndex = flag.String("index", "mail", "name of the ElasticSearch index")
var doInit = flag.Bool("init", false, "whether to initialize the index instead of indexing mail")

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
		addrs, err := mail.ParseAddressList(val)
		if err != nil {
			log.Printf("Could not parse address list: %s", val)
			result = append(result, val)
		} else {
			for _, addr := range addrs {
				result = append(result, addr.Address)
			}
		}
	}
	return result
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
	result.Header["From"] = normalizeAddrs(msg.Header["From"])
	result.Header["To"] = normalizeAddrs(msg.Header["To"])
	result.Header["Cc"] = normalizeAddrs(msg.Header["Cc"])
	result.Header["Bcc"] = normalizeAddrs(msg.Header["Bcc"])
	result.Header["Return-Path"] = normalizeAddrs(msg.Header["Return-Path"])
	result.Header["Delivered-To"] = normalizeAddrs(msg.Header["Delivered-To"])
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
	if strings.HasPrefix(msg.Header.Get("Content-Type"), "text") {
		_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
		if err != nil {
			log.Printf("Unreadable Content-Type: %s: %v", msg.Header.Get("Content-Type"), err)
			goto file
		}
		if params["charset"] == "" {
			log.Printf("No charset in Content-Type: %s, assuming UTF-8", msg.Header.Get("Content-Type"))
			params["charset"] = "utf-8"
		}
		if params["charset"] == "us-ascii" {
			params["charset"] = "utf-8"
		}
		enc, err := ianaindex.IANA.Encoding(params["charset"])
		if err != nil || enc == nil {
			log.Printf("Unknown encoding %s: %v", params["charset"], err)
			goto file
		}
		decoded, err := enc.NewDecoder().Bytes(msg.Body)
		if err != nil {
			log.Printf("Could not decode %s: %v", params["charset"], err)
			goto file
		}
		result.TextBody = string(decoded)
		return result
	}
file:
	hash := blake2b.Sum256(msg.Body)
	path := filepath.Join(*attachdir, hex.EncodeToString(hash[:]))
	result.Attachment = path
	err := ioutil.WriteFile(path, msg.Body, 0444)
	if err != nil {
		log.Printf("Could not write file %s: %v", path, err)
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
	flag.Parse()
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
						log.Fatalf("Error parsing envelope: %v ", err)
					}
					j, err := easyjson.Marshal(*jmsg)
					if err != nil {
						log.Fatalf("Error serializing JSON: %v", err)
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
