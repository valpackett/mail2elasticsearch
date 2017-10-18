# mail2elasticsearch [![unlicense](https://img.shields.io/badge/un-license-green.svg?style=flat)](http://unlicense.org)

A MIME email indexer for [ElasticSearch](https://www.elastic.co/products/elasticsearch), written in Go.

- preserves mail structure (nested parts)
- tells ElasticSearch to index dates correctly
- deduplicates attachments by storing them in a plain filesystem folder, using hashed contents as the filename (note: potential [attachment content indexing using other tools](https://blog.ambar.cloud/ingesting-documents-pdf-word-txt-etc-into-elasticsearch/))
- decodes [a ton of character sets](https://github.com/golang/text/blob/master/encoding/htmlindex/tables.go), with [autodetection](https://github.com/gogits/chardet) when needed
- is observable: uses [structured logging](https://github.com/uber-go/zap), optionally exposes profiling and stats over HTTP
- is fast: indexes multiple files in parallel, uses ElasticSearch's bulk index endpoint, [static JSON encoding](https://github.com/mailru/easyjson), SIMD accelerated [BLAKE2b hashing](https://github.com/minio/blake2b-simd) and [base64 decoding](https://github.com/myfreeweb/go-base64-simd)
- is (mostly) robust: tested on a large real-world mail archive, did not crash, most mail was parsed correctly, but some messages were skipped (weird EOFs, base64 and quoted-printable errors)

## Usage

```bash
$ mail2elasticsearch -h # check available flags
$ mail2elasticsearch -init # setup the index

$ mail2elasticsearch < /mail/cur/some.letter # stdin
$ mail2elasticsearch /mail/cur/some.letter /mail/cur/other.letter # paths
$ mail2elasticsearch /mail/cur # recursive walk (e.g. initial bulk indexing)
```

### Development

```bash
$ mail2elasticsearch -srvaddr 127.0.0.1:42069 -attachdir /tmp/files ~/testmail/cur 2>&1 | humanlog
```

Use

- [humanlog](https://github.com/aybabtme/humanlog) to read logs in development
- [go-torch](https://github.com/uber/go-torch) to profile with flamegraphs
- [expvarmon](https://github.com/divan/expvarmon) to monitor stats
- [gometalinter](https://github.com/alecthomas/gometalinter) to analyze code

## Contributing

Please feel free to submit pull requests!

By participating in this project you agree to follow the [Contributor Code of Conduct](http://contributor-covenant.org/version/1/4/).

[The list of contributors is available on GitHub](https://github.com/myfreeweb/mail2elasticsearch/graphs/contributors).

## License

This is free and unencumbered software released into the public domain.  
For more information, please refer to the `UNLICENSE` file or [unlicense.org](http://unlicense.org).
