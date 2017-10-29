//go:generate easyjson main.go
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	_ "expvar"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/quotedprintable"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/dchest/safefile"
	"github.com/mailru/easyjson"
	blake2b "github.com/minio/blake2b-simd"
	"github.com/myfreeweb/go-base64-simd/base64"
	"github.com/myfreeweb/go-email/email"
	zap "go.uber.org/zap"
	elastic "gopkg.in/olivere/elastic.v5"
)

var attachdir = flag.String("attachdir", "files", "path to the attachments directory")
var elasticUrl = flag.String("elastic", "http://127.0.0.1:9200", "URL of the ElasticSearch server")
var elasticIndex = flag.String("index", "mail", "name of the ElasticSearch index")
var doInit = flag.Bool("init", false, "whether to initialize the index instead of indexing mail")
var srvAddr = flag.String("srvaddr", "", "address for the pprof/expvar server to listen on")

const indexSettings string = `{
	"mappings": {
		"msg": {
			"properties": {
				"h": {
					"properties": {
						"Date": { "type": "date", "format": "EEE, dd MMM yyyy HH:mm:ss Z" },
						"Dkim-Signature": { "type": "text", "index": false },
						"X-Google-Dkim-Signature": { "type": "text", "index": false },
						"MIME-Version": { "type": "keyword" }
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

func jsonifyMsg(msg email.Message, log *zap.SugaredLogger) JMessage {
	log = log.With("msgid", msg.Header.Get("Message-Id"))
	wordDecoder := new(mime.WordDecoder)
	wordDecoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		return decodeReader(charset, input, log)
	}
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
	//// Headers
	delete(result.Header, "Message-Id")
	for k, vs := range result.Header {
		for i, v := range vs {
			dec, err := wordDecoder.DecodeHeader(v)
			if err != nil {
				log.Warnw("Could not decode header", "name", k, "index", i, "value", v, "err", err)
				continue
			}
			result.Header[k][i] = dec
		}
	}
	result.Header["Date"] = stripSpaceAndComments(result.Header["Date"])
	result.Header["From"] = splitAddrs(result.Header["From"])
	result.Header["To"] = splitAddrs(result.Header["To"])
	result.Header["Cc"] = splitAddrs(result.Header["Cc"])
	result.Header["Bcc"] = splitAddrs(result.Header["Bcc"])
	result.Header["Return-Path"] = splitAddrs(result.Header["Return-Path"])
	result.Header["Delivered-To"] = splitAddrs(result.Header["Delivered-To"])
	//// Parts
	if msg.SubMessage != nil {
		submsg := jsonifyMsg(*msg.SubMessage, log.With("submsg", true))
		result.SubMessage = &submsg
	}
	for partidx, part := range msg.Parts {
		if part != nil {
			partmsg := jsonifyMsg(*part, log.With("partidx", partidx))
			result.Parts = append(result.Parts, &partmsg)
		}
	}
	//// Body
	ctype := result.Header.Get("Content-Type")
	//// Body Transfer-Encoding
	if result.Header.Get("Content-Transfer-Encoding") == "quoted-printable" {
		decBody, err := ioutil.ReadAll(quotedprintable.NewReader(bytes.NewReader(msg.Body)))
		if err != nil {
			log.Warnw("Could not decode quoted-printable, treating like an attachment", "err", err)
			goto file
		}
		msg.Body = decBody
	} else if result.Header.Get("Content-Transfer-Encoding") == "base64" {
		unspacedBody := normalizeForBase64(msg.Body)
		decBody := make([]byte, base64.StdEncoding.DecodedLen(len(unspacedBody)))
		n, err := base64.StdEncoding.Decode(decBody, unspacedBody)
		if err != nil {
			log.Warnw("Could not decode base64, treating like an attachment", "nbytes", n, "err", err)
			goto file
		}
		msg.Body = decBody
	}
	//// Body Charset
	if strings.HasPrefix(ctype, "text") && !strings.Contains(result.Header.Get("Content-Disposition"), "attachment") {
		mediatype, params, err := mime.ParseMediaType(ctype)
		if err != nil {
			if strings.Contains(ctype, "html") {
				mediatype = "text/html"
			} else {
				mediatype = "text/plain"
			}
			params = make(map[string]string)
			log.Warnw("Unreadable Content-Type", "ctype", ctype, "err", err, "assumed", mediatype)
		}
		decoded, charset, err := decodeCharset(
			params["charset"],
			msg.Body,
			fmt.Sprintf("Content-Type: %s", ctype),
			strings.Contains(mediatype, "html"),
			log)
		if err != nil {
			log.Warnw("Could not decode charset, treating like an attachment", "charset", charset, "err", err)
			goto file
		}
		result.TextBody = string(decoded)
		return result
	}
file:
	hash := blake2b.Sum256(msg.Body)
	path := filepath.Join(*attachdir, hex.EncodeToString(hash[:]))
	log = log.With("path", path)
	result.Attachment = path
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		log.Debug("Attachment already exists")
		return result
	}
	f, err := safefile.Create(path, 0444)
	if err != nil {
		log.Errorw("Could not open file for attachment", "err", err)
		return result
	}
	defer f.Close()
	_, err = f.Write(msg.Body)
	if err != nil {
		log.Errorw("Could not write attachment", "err", err)
		return result
	}
	err = f.Commit()
	if err != nil {
		log.Errorw("Could not commit attachment", "err", err)
		return result
	}
	log.Info("Saved attachment")
	return result
}

func process(msgtext io.Reader, log *zap.SugaredLogger) (*JMessage, error) {
	msg, err := email.ParseMessage(msgtext)
	if err != nil {
		return nil, err
	}
	jmsg := jsonifyMsg(*msg, log)
	return &jmsg, nil
}

func main() {
	flag.Parse()
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	log := logger.Sugar()
	if *srvAddr != "" {
		go func() {
			log.Infow("pprof/expvar server started", "result", http.ListenAndServe(*srvAddr, nil))
		}()
	}
	ctx := context.Background()
	client, err := elastic.NewClient(
		elastic.SetURL(*elasticUrl),
	)
	if err != nil {
		log.Fatalw("Could not create ElasticSearch client", "err", err)
	}
	if *doInit {
		res, err := client.CreateIndex(*elasticIndex).BodyString(indexSettings).Do(ctx)
		if err != nil {
			log.Fatalw("Could not initialize index", "err", err)
		} else {
			log.Infow("Created index", "result", res)
		}
	} else if len(flag.Args()) == 0 || flag.Arg(0) == "-" {
		jmsg, err := process(bufio.NewReader(os.Stdin), log.With("filename", "stdin"))
		if err != nil {
			log.Fatalw("Could not process", "err", err)
		}
		j, err := easyjson.Marshal(*jmsg)
		if err != nil {
			log.Fatalw("Could not serialize JSON", "err", err)
		}
		_, err = client.Index().Index(*elasticIndex).Type("msg").Id(jmsg.Id).BodyString(string(j)).Do(ctx)
		if err != nil {
			log.Fatalw("Could not index", "err", err)
		}
	} else {
		proc, err := client.BulkProcessor().Name("mail2elasticsearch").Do(ctx)
		if err != nil {
			log.Fatalw("Could not start bulk processor", "err", err)
		}
		defer proc.Close()
		var wg sync.WaitGroup
		tasks := make(chan string)
		for i := 0; i < runtime.GOMAXPROCS(0); i++ {
			go func() {
				for {
					var j []byte
					var jmsg *JMessage
					filename := <-tasks
					log := log.With("filename", filename)
					log.Debug("Processing start")
					file, err := os.Open(filename)
					if err != nil {
						log.Errorw("Could not open file", "err", err)
						goto done
					}
					jmsg, err = process(bufio.NewReader(file), log)
					if err != nil {
						log.Errorw("Could not process", "err", err)
						goto done
					}
					j, err = easyjson.Marshal(*jmsg)
					if err != nil {
						log.Errorw("Could not serialize JSON", "err", err)
						goto done
					}
					proc.Add(elastic.NewBulkIndexRequest().Index(*elasticIndex).Type("msg").Id(jmsg.Id).Doc(string(j)))
					log.Debug("Processing end")
				done:
					wg.Done()
				}
			}()
		}
		for _, filename := range flag.Args() {
			f, err := os.Stat(filename)
			if err != nil {
				log.Fatalw("Could not stat file", "err", err, "filename", filename)
			}
			if f.Mode().IsDir() {
				err = filepath.Walk(filename, func(path string, _ os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					f, err := os.Stat(path)
					if err != nil {
						log.Fatalw("Could not stat file", "err", err, "filename", path)
					}
					if f.Mode().IsRegular() {
						wg.Add(1)
						tasks <- path
					} else {
						log.Infow("Not a file", "filename", path)
					}
					return nil
				})
				if err != nil {
					log.Fatalw("Could not walk directory", "err", err, "filename", filename)
				}
			} else {
				wg.Add(1)
				tasks <- filename
			}
		}
		wg.Wait()
	}
}

func normalizeForBase64(body []byte) []byte {
	// SIMD-accelerated base64 can't skip over extra crap
	return bytes.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '+' || r == '/' || r == '=' {
			return r
		}
		if r == ',' || r == '_' || r == ':' {
			return '/'
		}
		if r == '-' {
			return '+'
		}
		return -1
	}, body)
}
