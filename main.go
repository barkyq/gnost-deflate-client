package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gobwas/httphead"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsflate"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip42"
)

var scheme = flag.String("scheme", "wss", "ws or wss")
var hostname = flag.String("host", "nos.lol", "relay hostname")
var port = flag.Int("port", 443, "remote TCP port")
var output = flag.String("output", "events.jsonl", "output file. use - for stdout")
var keepalive = flag.Int64("keepalive", 0, "keep connection alive (set to number of seconds between sending pings to relay)")

// Read Buffer Size
const RBS = 1024

func main() {
	flag.Parse()
	f := make(nostr.Filters, 0)
	dec := json.NewDecoder(os.Stdin)
	if err := dec.Decode(&f); err != nil {
		panic(err)
	}
	logger := log.New(os.Stderr, "(gnost-deflate-client) ", log.LstdFlags|log.Lmsgprefix)
	nostr_handler(*output, *scheme, *hostname, *port, *keepalive, f, logger)
}

func nostr_handler(output string, scheme string, hostname string, port int, keepalive int64, filters nostr.Filters, logger *log.Logger) {
	u, err := url.Parse(fmt.Sprintf("%s://%s:%d", scheme, hostname, port))
	if err != nil {
		panic(err)
	}

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

	info, e := NIP11_fetch(conn, hostname)
	if e != nil {
		panic(e)
	}
	logger.Printf("software: %s\n", info.Software)

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
	if permessage_deflate {
		var ex string
		ex += "compression: permessage-deflate"
		if server_nct {
			ex += "; server_no_context_takeover"
		}
		if client_nct {
			ex += "; client_no_context_takeover"
		}
		logger.Println(ex)
	} else {
		logger.Println("relay does not implement permessage_deflate.")
	}

	json_msgs := make(chan Message, 5)
	ctx, cancel := context.WithCancel(context.Background())
	flush_bytes := [4]byte{0x00, 0x00, 0xff, 0xff}

	var writer io.WriteCloser // data to writer is compressed (if negotiated) and sent to reader
	var reader io.Reader      // data to reader is framed and sent to websocket connection

	// compression handler
	if permessage_deflate {
		rp, wp := io.Pipe()
		rpp, wpp := io.Pipe()
		reader = rpp
		writer = wp
		go func() {
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
						return
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
					if e := ws.WriteFrame(conn, ws.MaskFrame(ws.NewCloseFrame(ws.NewCloseFrameBody(ws.StatusNormalClosure, "")))); e != nil {
						panic(e)
					}
					time.Sleep(10 * time.Second)
					if cl, ok := conn.(io.Closer); ok == true {
						cl.Close()
					}
					return
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
		if e := websocket_receive_handler(logger, permessage_deflate, server_nct, br, conn, json_msgs); e != nil {
			if e == io.EOF || errors.Is(e, net.ErrClosed) {
				logger.Println("relay closed the connection ungracefully.")
			} else {
				panic(e)
			}
		}
	}()

	var notice string
	var challenge string
	var ok bool

	// send the message
	if len(filters) > 0 {
		go func() {
			buf := bytes.NewBuffer(nil)
			enc := json.NewEncoder(buf)
			enc.SetEscapeHTML(false)

			b := make([]byte, 7)
			rand.Reader.Read(b)
			subid := fmt.Sprintf("%x", b[0:7])

			send := []interface{}{"REQ", subid}
			for _, f := range filters {
				send = append(send, f)
			}

			if err := enc.Encode(send); err != nil {
				panic(err)
			}
			writer.Write(buf.Bytes())
			writer.Write(flush_bytes[:])
		}()
	}
	// handle returned messages
	nip42_auth := sync.Once{}
	var file_enc *json.Encoder
	if output == "-" {
		file_enc = json.NewEncoder(os.Stdout)
	} else {
		if f, err := os.Create(output); err == nil {
			defer f.Close()
			file_enc = json.NewEncoder(f)
		} else {
			panic(err)
		}
	}
	file_enc.SetEscapeHTML(false)

	// count the returned notes:
	var counter int

	if keepalive > 0 {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		ticker := time.NewTicker(time.Duration(keepalive) * time.Second)
		frame := ws.MaskFrame(ws.NewPingFrame([]byte(nil)))
		go func() {
			for {
				select {
				case <-ticker.C:
					if e := ws.WriteFrame(conn, frame); e != nil {
						writer.Close()
						return
					}
				case <-sigs:
					fmt.Println()
					writer.Close()
					return
				}
			}
		}()
	}

	// handle received json messages
	for {
		select {
		case <-ctx.Done():
			// the websocket receive handler has returned
			if keepalive > 0 {
				logger.Printf("total: %d events written to %s\n", counter, output)
			}
			return
		case msg := <-json_msgs:
			switch [2]byte{msg.jmsg[0][1], msg.jmsg[0][2]} {
			case [2]byte{'E', 'O'}:
				if err := json.Unmarshal(msg.jmsg[1], &challenge); err != nil {
					panic(err)
				}
				if keepalive == 0 {
					writer.Close()
				}
				logger.Printf("EOSE: %d events written to %s\n", counter, output)
			case [2]byte{'E', 'V'}:
				counter++
				file_enc.Encode(msg.jmsg[2])
			case [2]byte{'N', 'O'}:
				json.Unmarshal(msg.jmsg[1], &notice)
				logger.Printf("NOTICE: %s\n", notice)
			case [2]byte{'O', 'K'}:
				json.Unmarshal(msg.jmsg[1], &challenge)
				json.Unmarshal(msg.jmsg[2], &ok)
				json.Unmarshal(msg.jmsg[3], &notice)
				logger.Printf("OK: %s %t %s\n", challenge, ok, notice)
			case [2]byte{'A', 'U'}:
				nip42_auth.Do(func() {
					sk := nostr.GeneratePrivateKey()
					pk, err := nostr.GetPublicKey(sk)
					if err != nil {
						panic(err)
					}
					buf := bytes.NewBuffer(nil)
					enc := json.NewEncoder(buf)
					enc.SetEscapeHTML(false)
					json.Unmarshal(msg.jmsg[1], &challenge)
					event := nip42.CreateUnsignedAuthEvent(challenge, pk, scheme+"://"+hostname)
					event.Sign(sk)
					reply := []interface{}{"AUTH", event}
					if err = enc.Encode(reply); err != nil {
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

func websocket_receive_handler(logger *log.Logger, permessage_deflate bool, server_nct bool, br *bufio.Reader, conn io.ReadWriter, json_msgs chan Message) error {
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

	// counters
	var downloaded int64
	var decompressed int

	if permessage_deflate {
		// log the compression stats
		defer func() {
			comp_f := (&big.Float{}).SetInt64(downloaded)
			decomp_f := (&big.Float{}).SetInt64(int64(decompressed))
			logger.Printf("bytes downloaded (compressed): %.0f\n", comp_f)
			logger.Printf("bytes downloaded (decompressed): %.0f\n", decomp_f)
			if decomp_f.Cmp(comp_f) == +1 {
				comp_f.Mul(comp_f, big.NewFloat(100))
				logger.Printf("compression ratio: %.0f%%\n", comp_f.Quo(comp_f, decomp_f))
			}
		}()
	}

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
			return e
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
				if n, err := io.ReadFull(control, b[:]); err != nil || n != 2 {
					panic(err)
				}
				logger.Printf("closed with status code: %d", int(b[0])*256+int(b[1]))
				return nil
			case ws.OpPing:
				frame := ws.MaskFrame(ws.NewPongFrame(control.Bytes()))
				logger.Printf("PING received")
				ws.WriteFrame(conn, frame)
			case ws.OpPong:
				logger.Printf("PONG received")
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
