package syslog

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/segmentio/ecs-logs/lib"
)

const (
	DefaultTemplate = "<{{.PRIVAL}}>{{.TIMESTAMP}} {{.GROUP}}[{{.STREAM}}]: {{.MSG}}"
)

type WriterConfig struct {
	Network    string
	Address    string
	Template   string
	TimeFormat string
	TLS        *tls.Config
}

func NewWriter(group string, stream string) (ecslogs.Writer, error) {
	return DialWriter(WriterConfig{})
}

func DialWriter(config WriterConfig) (w ecslogs.Writer, err error) {
	var netopts []string
	var addropts []string
	var backend io.Writer

	if len(config.Network) != 0 {
		netopts = []string{config.Network}
		addropts = []string{config.Address}
	} else {
		// This was copied from the standard log/syslog package, they do the same
		// and try to guess at runtime where and what kind of socket syslogd is
		// using.
		netopts = []string{"unixgram", "unix"}
		addropts = []string{"/dev/log", "/var/run/syslog", "/var/run/log"}
	}

connect:
	for _, n := range netopts {
		for _, a := range addropts {
			if backend, err = dialWriter(n, a, config.TLS); err == nil {
				break connect
			}
		}
	}

	if err != nil {
		return
	}

	w = newWriter(writerConfig{
		backend:    backend,
		template:   config.Template,
		timeFormat: config.TimeFormat,
	})
	return
}

type writerConfig struct {
	backend    io.Writer
	template   string
	timeFormat string
}

func newWriter(config writerConfig) *writer {
	var out func(*writer, message) error
	var flush func() error

	if len(config.timeFormat) == 0 {
		config.timeFormat = time.Stamp
	}

	if len(config.template) == 0 {
		config.template = DefaultTemplate
	}

	switch b := config.backend.(type) {
	case bufferedWriter:
		out, flush = (*writer).directWrite, b.Flush
	default:
		out, flush = (*writer).bufferedWrite, func() error { return nil }
	}

	return &writer{
		writerConfig: config,
		flush:        flush,
		out:          out,
		tpl:          newWriterTemplate(config.template),
	}
}

func newWriterTemplate(format string) *template.Template {
	if !strings.HasSuffix(format, "\n") {
		format += "\n"
	}
	t := template.New("syslog")
	template.Must(t.Parse(format))
	return t
}

type writer struct {
	writerConfig
	buf   bytes.Buffer
	tpl   *template.Template
	out   func(*writer, message) error
	flush func() error
}

func (w *writer) Close() (err error) {
	if c, ok := w.backend.(io.Closer); ok {
		err = c.Close()
	}
	return
}

func (w *writer) WriteMessageBatch(batch []ecslogs.Message) (err error) {
	for _, msg := range batch {
		if err = w.write(msg); err != nil {
			return
		}
	}
	return w.flush()
}

func (w *writer) WriteMessage(msg ecslogs.Message) (err error) {
	if err = w.write(msg); err == nil {
		err = w.flush()
	}
	return
}

func (w *writer) write(msg ecslogs.Message) (err error) {
	m := message{
		PRIVAL:    int(msg.Level) + 8, // +8 is for user-level messages facility
		HOSTNAME:  msg.Host,
		MSGID:     msg.ID,
		GROUP:     msg.Group,
		STREAM:    msg.Stream,
		MSG:       msg.Content,
		TIMESTAMP: msg.Time.Format(w.timeFormat),
	}

	if len(m.HOSTNAME) == 0 {
		m.HOSTNAME = "-"
	}

	if len(m.MSGID) == 0 {
		m.MSGID = "-"
	}

	if msg.PID == 0 {
		m.PROCID = "-"
	} else {
		m.PROCID = strconv.Itoa(msg.PID)
	}

	if len(msg.File) != 0 || len(msg.Func) != 0 {
		m.SOURCE = fmt.Sprintf("%s:%s:%d", msg.File, msg.Func, msg.Line)
	}

	return w.out(w, m)
}

func (w *writer) directWrite(m message) (err error) {
	return w.tpl.Execute(w.backend, m)
}

func (w *writer) bufferedWrite(m message) (err error) {
	w.buf.Reset()
	w.tpl.Execute(&w.buf, m)
	_, err = w.backend.Write(w.buf.Bytes())
	return
}

type message struct {
	PRIVAL    int
	HOSTNAME  string
	PROCID    string
	MSGID     string
	GROUP     string
	STREAM    string
	MSG       string
	SOURCE    string
	TIMESTAMP string
}

type bufferedWriter interface {
	Flush() error
}

type bufferedConn struct {
	buf  *bufio.Writer
	conn net.Conn
}

func (c bufferedConn) Close() error                { return c.conn.Close() }
func (c bufferedConn) Flush() error                { return c.buf.Flush() }
func (c bufferedConn) Write(b []byte) (int, error) { return c.buf.Write(b) }

func dialWriter(network string, address string, config *tls.Config) (w io.Writer, err error) {
	var conn net.Conn

	if network == "tls" {
		conn, err = tls.Dial("tcp", address, config)
	} else {
		conn, err = net.Dial(network, address)
	}

	if err == nil {
		switch network {
		case "udp", "udp4", "udp6", "unixgram", "unixpacket":
			w = conn
		default:
			w = bufferedConn{
				conn: conn,
				buf:  bufio.NewWriter(conn),
			}
		}
	}

	return
}
