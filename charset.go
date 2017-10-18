package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"

	"github.com/gogits/chardet"
	"go.uber.org/zap"
	"golang.org/x/text/encoding/htmlindex"
)

var htmlDetector = chardet.NewHtmlDetector()
var textDetector = chardet.NewTextDetector()

func decodeCharset(charset string, body []byte, description string, ishtml bool, log *zap.SugaredLogger) ([]byte, string, error) {
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
			log.Infow("Using detected charset", "where", description, "detected", detenc.Charset,
				"lang", detenc.Language, "confidence", detenc.Confidence)
		} else {
			charset = "utf-8"
			log.Infow("Could not detect charset, assuming UTF-8", "where", description)
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

func decodeReader(charset string, input io.Reader, log *zap.SugaredLogger) (io.Reader, error) {
	body, err := ioutil.ReadAll(input)
	if err != nil {
		return nil, err
	}
	decoded, _, err := decodeCharset(charset, body, fmt.Sprintf("header '%s'", body), false, log)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(decoded), nil
}
