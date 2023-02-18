package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/nbd-wtf/go-nostr/nip11"
)

func NIP11_fetch(conn io.ReadWriter, hostname string) (*nip11.RelayInformationDocument, error) {
	wb := bufio.NewWriter(conn)
	wb.Write([]byte("GET / HTTP/1.1\r\n"))
	wb.Write([]byte(fmt.Sprintf("Host: %s\r\n", hostname)))
	wb.Write([]byte("User-Agent: barkyq-http-client/1.0\r\n"))
	wb.Write([]byte("Accept: application/nostr+json\r\n"))
	wb.Write([]byte("Accept-Encoding: gzip\r\n"))
	wb.Write([]byte("\r\n"))
	wb.Flush()

	rb := bufio.NewReader(conn)
	// read the headers
	var chunked bool
	var content_encoding string
	for {
		header_line, err := rb.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if arr := strings.Split(header_line, ":"); len(arr) > 1 {
			key := strings.TrimSpace(strings.ToLower(arr[0]))
			val := strings.TrimSpace(strings.ToLower(arr[1]))
			switch key {
			case "transfer-encoding":
				if val == "chunked" {
					chunked = true
				}
			case "content-encoding":
				content_encoding = val
			default:
			}
		}
		if header_line == "\r\n" {
			// break at the empty CRLF
			break
		}
	}
	var nip11_reader io.Reader
	if chunked {
		var tmp [32]byte
		data_buf := bytes.NewBuffer(nil)
		for {
			chunk, err := rb.ReadString('\n')
			if err != nil {
				return nil, err
			}
			chunk_size, err := strconv.ParseInt(strings.TrimSpace(chunk), 16, 64)
			if err != nil {
				return nil, err
			}
			if chunk_size == 0 {
				rb.Discard(2)
				// finished chunking
				break
			}
			for chunk_size > 32 {
				if n, err := rb.Read(tmp[:]); err == nil {
					chunk_size -= int64(n)
					data_buf.Write(tmp[:n])
				} else {
					return nil, err
				}
			}
			if n, err := rb.Read(tmp[:chunk_size]); err == nil {
				data_buf.Write(tmp[:n])
			}
			// chunk size does not account for CRLF added to end of chunk data
			rb.Discard(2)
		}
		nip11_reader = data_buf
	} else {
		nip11_reader = rb
	}
	var dec *json.Decoder
	switch content_encoding {
	case "gzip":
		if r, err := gzip.NewReader(nip11_reader); err != nil {
			return nil, err
		} else {
			dec = json.NewDecoder(r)
		}
	default:
		dec = json.NewDecoder(nip11_reader)
	}
	info := nip11.RelayInformationDocument{}
	if e := dec.Decode(&info); e != nil {
		return nil, e
	}
	if rb.Buffered() != 0 {
		return &info, fmt.Errorf("unread bytes trapped in bufio.Reader")
	}
	return &info, nil
}
