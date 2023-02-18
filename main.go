package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/tls"
	"math/big"
	"os"

	"sync"

	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"

	"github.com/gobwas/httphead"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsflate"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip42"
)

var scheme = flag.String("scheme", "ws", "ws or wss")
var hostname = flag.String("host", "localhost", "port")
var port = flag.Int("port", 8080, "port")
var output = flag.String("output", "events.jsonl", "output file")

// Read Buffer Size
const RBS = 1024

func main() {
	flag.Parse()
	f := nostr.Filter{}
	dec := json.NewDecoder(os.Stdin)
	if err := dec.Decode(&f); err != nil {
		panic(err)
	}
	nostr_handler(*output, *scheme, *hostname, *port, nostr.Filters{f})
}

func nostr_handler(output string, scheme string, hostname string, port int, filters nostr.Filters) {
	var conn io.ReadWriter
	switch scheme {
	case "ws":
		if c, err := net.Dial("tcp", fmt.Sprintf("%s:%d", hostname, port)); err != nil {
			panic(err)
		} else {
			conn = c
		}
	case "wss":
		if c, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", hostname, port), &tls.Config{ServerName: hostname}); err != nil {
			panic(err)
		} else {
			conn = c
		}
	default:
		panic("invalid scheme")
	}
	u, err := url.Parse(fmt.Sprintf("%s://%s:%d", scheme, hostname, port))
	if err != nil {
		panic(err)
	}
	head := make(ws.HandshakeHeaderHTTP)
	head["User-Agent"] = []string{"barkyq-websocket-client/1.0"}

	// these parameters will be negotiated in the extension
	p := wsflate.Parameters{
		ServerNoContextTakeover: false, // default: server can reuse LZ77 buffer
		ClientNoContextTakeover: false, // default: client can reuse LZ77 buffer
	}
	d := ws.Dialer{
		Header:     head,
		Extensions: []httphead.Option{p.Option()},
	}
	br, handshake, err := d.Upgrade(conn, u)
	if err != nil {
		panic(err)
	}

	var permessage_deflate, server_nct, client_nct bool
	for _, opt := range handshake.Extensions {
		target := []byte("permessage-deflate")
		if len(opt.Name) != len(target) {
			continue
		}
		permessage_deflate = true
		for i, b := range opt.Name {
			if b != target[i] {
				goto jump
			}
		}
		_, client_nct = opt.Parameters.Get("client_no_context_takeover")
		_, server_nct = opt.Parameters.Get("server_no_context_takeover")
	jump:
	}
	fmt.Printf("permessage_deflate: %t\n", permessage_deflate)
	if permessage_deflate {
		fmt.Printf("server_no_context_takeover: %t\nclient_no_context_takeover: %t\n", server_nct, client_nct)
	}

	json_msgs := make(chan Message, 5)
	ctx, cancel := context.WithCancel(context.Background())

	if err != nil {
		panic(err)
	}
	flush_bytes := [4]byte{0x00, 0x00, 0xff, 0xff}

	var writer io.WriteCloser // date to writer is compressed (if negotiated) and sent to reader
	var reader io.Reader      // data in reader is framed and sent to websocket connection

	// compression handler
	if permessage_deflate {
		rp, wp := io.Pipe()
		rpp, wpp := io.Pipe()
		reader = rpp
		writer = wp
		go func() {
			var flate_writer *flate.Writer
			buf := make([]byte, RBS)
			flate_writer, err := flate.NewWriter(wpp, flate.BestCompression)
			if err != nil {
				panic(err)
			}
			for {
				for {
					n, e := rp.Read(buf)
					switch {
					case e == nil:
					case e == io.EOF:
						wpp.Close()
					default:
						panic(e)
					}
					if n == 4 && buf[0] == 0x00 && buf[1] == 0x00 && buf[2] == 0xff && buf[3] == 0xff {
						switch {
						case client_nct:
							e = flate_writer.Close()
							flate_writer.Reset(wpp)
						default:
							flate_writer.Flush()
						}
						break
					}
					if _, e = flate_writer.Write(buf[:n]); e != nil {
						panic(e)
					}
				}
			}
		}()
	} else {
		// wp -> rp copy
		rp, wp := io.Pipe()
		reader = rp
		writer = wp
	}

	// sending handler
	go func() {
		payload := bytes.NewBuffer(nil)
		buf := make([]byte, RBS)
		for {
			for {
				n, e := reader.Read(buf)
				switch {
				case e == nil:
				case e == io.EOF:
					ws.WriteFrame(conn, ws.NewCloseFrame(ws.NewCloseFrameBody(ws.StatusNormalClosure, "leaving")))
				default:
					panic(e)
				}
				// test for flush
				// no edge case, since there will never be a valid send of an empty uncompressed block
				if n == 4 && buf[0] == 0x00 && buf[1] == 0x00 && buf[2] == 0xff && buf[3] == 0xff {
					break
				}
				payload.Write(buf[:n])
			}
			fr := ws.NewTextFrame(payload.Bytes())
			if permessage_deflate {
				fr.Header.Rsv = ws.Rsv(true, false, false)
			}
			fr = ws.MaskFrame(fr)
			ws.WriteFrame(conn, fr)
			payload.Reset()
		}
	}()

	// receiving handler
	go func() {
		defer cancel()
		if e := payload_conn_handler(permessage_deflate, server_nct, br, conn, json_msgs); e != nil {
			panic(e)
		}
	}()

	var notice string
	var challenge string
	var ok bool
	sk := nostr.GeneratePrivateKey()
	pk, e := nostr.GetPublicKey(sk)
	if e != nil {
		panic(e)
	}

	// send the REQ
	go func() {
		buf := bytes.NewBuffer(nil)
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		for i := 0; i < 1; i++ {
			func() {
				b := make([]byte, 7)
				rand.Reader.Read(b)
				subid := fmt.Sprintf("%x", b[0:7])

				send := []interface{}{"REQ", subid}
				for _, f := range filters {
					send = append(send, f)
				}
				buf.Reset()
				if err := enc.Encode(send); err != nil {
					panic(err)
				}
				writer.Write(buf.Bytes())
				writer.Write(flush_bytes[:])
			}()
		}
	}()

	// handle returned messages
	nip42_auth := sync.Once{}
	var file_enc *json.Encoder
	var close func() error
	if f, err := os.Create(output); err == nil {
		file_enc = json.NewEncoder(f)
		close = f.Close
		file_enc.SetEscapeHTML(false)
	} else {
		panic(err)
	}
	var counter int
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-json_msgs:
			switch [2]byte{msg.jmsg[0][1], msg.jmsg[0][2]} {
			case [2]byte{'E', 'O'}:
				if err := json.Unmarshal(msg.jmsg[1], &challenge); err != nil {
					panic(err)
				}
				close()
				writer.Close()
				fmt.Printf("EOSE: %d events returned\n", counter)
			case [2]byte{'E', 'V'}:
				counter++
				file_enc.Encode(msg.jmsg[2])
			case [2]byte{'N', 'O'}:
				json.Unmarshal(msg.jmsg[1], &notice)
				fmt.Printf("NOTICE: %s\n", notice)
			case [2]byte{'O', 'K'}:
				json.Unmarshal(msg.jmsg[1], &challenge)
				json.Unmarshal(msg.jmsg[2], &ok)
				json.Unmarshal(msg.jmsg[3], &notice)
				fmt.Printf("OK: %s %t %s\n", challenge, ok, notice)
			case [2]byte{'A', 'U'}:
				nip42_auth.Do(func() {
					buf := bytes.NewBuffer(nil)
					enc := json.NewEncoder(buf)
					enc.SetEscapeHTML(false)
					json.Unmarshal(msg.jmsg[1], &challenge)
					event := nip42.CreateUnsignedAuthEvent(challenge, pk, scheme+"://"+hostname)
					event.Sign(sk)
					reply := []interface{}{"AUTH", event}
					if err := enc.Encode(reply); err != nil {
						panic(err)
					}
					writer.Write(buf.Bytes())
					writer.Write(flush_bytes[:])
					buf.Reset()
				})
			}
			msg.Release()
		}
	}
}

type Message struct {
	jmsg []json.RawMessage
	pool *sync.Pool
}

func (msg *Message) Release() {
	msg.pool.Put(msg.jmsg)
}

func payload_conn_handler(permessage_deflate bool, server_nct bool, br *bufio.Reader, conn io.ReadWriter, json_msgs chan Message) error {
	// r0 is for reading from connection (before unmasking, etc)
	// fr is flate reader (decompression)
	var r0, fr io.Reader
	var decoder, fr_decoder *json.Decoder
	payload := bytes.NewBuffer(nil)
	control := bytes.NewBuffer(nil)
	buf := make([]byte, RBS)

	// may need to use this, even if permessage_deflate has been negotiated
	// so allocate:
	payload_decoder := json.NewDecoder(payload)

	if permessage_deflate {
		// only allocate these if needed
		fr = flate.NewReader(payload)
		fr_decoder = json.NewDecoder(fr)
	}

	json_msg_pool := sync.Pool{
		New: func() any {
			return make([]json.RawMessage, 0)
		},
	}

	var downloaded int64
	var decompressed int

	var header ws.Header
	var remaining int64
	var compressed bool
	// the handshake can capture some of the bytes!
	if br != nil {
		r0 = br
	} else {
		r0 = conn
	}
	// main handler
	for {
		if h, e := ws.ReadHeader(r0); e == nil {
			header = h
		} else {
			switch e {
			case io.EOF:
				return e
			default:
				panic(e)
			}
		}
		if header.OpCode != ws.OpContinuation {
			compressed = header.Rsv1()
		}
		remaining = header.Length
		for remaining > 0 {
			var n int
			if remaining > RBS {
				n, _ = r0.Read(buf[:])
			} else {
				n, _ = r0.Read(buf[:remaining])
			}
			remaining -= int64(n)
			if header.Masked {
				ws.Cipher(buf[:n], header.Mask, 0)
			}
			// control frames can be interjected
			if !header.OpCode.IsControl() {
				payload.Write(buf[:n])
			} else {
				control.Write(buf[:n])
			}
		}
		if !header.Fin {
			continue
		}

		//Flush Control
		if header.OpCode.IsControl() {
			switch header.OpCode {
			case ws.OpClose:
				var b [2]byte // status code
				io.ReadFull(control, b[:])
				if permessage_deflate {
					comp_f := (&big.Float{}).SetInt64(downloaded)
					decomp_f := (&big.Float{}).SetInt64(int64(decompressed))
					fmt.Printf("downloaded (compressed): %.0f\n", comp_f)
					fmt.Printf("downloaded (decompressed): %.0f\n", decomp_f)
					comp_f.Mul(comp_f, big.NewFloat(100))
					fmt.Printf("compression ratio: %.0f%%\n", comp_f.Quo(comp_f, decomp_f))
				}
				fmt.Println("closed with status code:", int(b[0])*256+int(b[1]))
				return nil
			case ws.OpPing:
				ctrl_bytes, _ := io.ReadAll(control)
				frame := ws.NewPongFrame(ctrl_bytes)
				ws.WriteFrame(conn, frame)
			case ws.OpPong:
			}
			control.Reset()
			continue
		}

		// handle payload
		downloaded += header.Length
		if compressed {
			payload.Write([]byte{0x00, 0x00, 0xff, 0xff})
			decoder = fr_decoder
		} else {
			decoder = payload_decoder
		}
		json_msg := json_msg_pool.Get().([]json.RawMessage)
		if err := decoder.Decode(&json_msg); err != nil {
			panic(err)
		}
		for _, k := range json_msg {
			decompressed += len(k)
		}
		json_msgs <- Message{jmsg: json_msg, pool: &json_msg_pool}
		if r, ok := fr.(flate.Resetter); server_nct && ok == true {
			r.Reset(payload, nil)
		}
		if br, ok := r0.(*bufio.Reader); ok == true {
			ws.PutReader(br)
			r0 = conn
		}
	}
}
