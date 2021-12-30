package list

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"golang.org/x/crypto/blake2b"
)

type Worker struct {
	Verbose bool
	Store   string
}

func (w *Worker) log(f string, v ...interface{}) {
	if !w.Verbose {
		return
	}
	log.Printf(f, v...)
}
func (w *Worker) List(ctx context.Context, server, username, password string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c, err := client.DialTLS(server, nil)
	if err != nil {
		return err
	}

	if err := c.Login(username, password); err != nil {
		return fmt.Errorf("login to %v: %w", server, err)
	}
	defer c.Logout()

	miList := make([]*imap.MailboxInfo, 0, 100)

	errC := make(chan error)
	ch := make(chan *imap.MailboxInfo, 10)

	go func() {
		errC <- c.List("*", "*", ch)
	}()
	for mi := range ch {
		miList = append(miList, mi)
	}
	select {
	case <-ctx.Done():
	case err := <-errC:
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
	}

	for _, mi := range miList {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := w.Iter(ctx, c, mi)
		if err != nil {
			return fmt.Errorf("iter: %w", err)
		}
	}
	return c.Logout()
}

func (w *Worker) Iter(ctx context.Context, c *client.Client, mi *imap.MailboxInfo) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w.log("Folder: %s", mi.Name)

	_, err := c.Select(mi.Name, true)
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}

	seqset, err := imap.ParseSeqSet("1:*")
	if err != nil {
		return err
	}

	const keySize = 32
	key := [keySize]byte{}
	xof, err := blake2b.NewXOF(keySize, nil)
	if err != nil {
		return err
	}

	msgList := make([]uint32, 0, 100)
	msgC := make(chan *imap.Message, 10)
	fetchErr := make(chan error)
	go func() {
		fetchErr <- c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid}, msgC)
	}()
	existCount := 0
	for msg := range msgC {
		name, err := fn(xof, key[:], msg.Envelope.MessageId)
		if err != nil {
			return fmt.Errorf("fn: %w", err)
		}

		_, err = os.Stat(filepath.Join(w.Store, name))
		if err == nil {
			existCount++
			continue
		}
		if os.IsNotExist(err) {
			msgList = append(msgList, msg.SeqNum)
			continue
		}
		return fmt.Errorf("store stat: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-fetchErr:
		if err != nil {
			e := err.Error()
			switch {
			default:
				return fmt.Errorf("fetch: %w", err)
			case strings.Contains(e, "No matching messages"):
				w.log("\tno-messages")
				return nil
			}
		}
	}

	w.log("\tfetch %05d messages", len(msgList))
	w.log("\texist %05d messages", existCount)
	if len(msgList) == 0 {
		w.log("\tnothing-to-do")
		return nil
	}

	secName, err := imap.ParseBodySectionName(imap.FetchItem("BODY[]"))
	if err != nil {
		return err
	}

	ss := &imap.SeqSet{}
	for _, v := range msgList {
		ss.AddNum(v)
	}
	msgC = make(chan *imap.Message, 10)
	go func() {
		fetchErr <- c.Fetch(ss, []imap.FetchItem{imap.FetchEnvelope, secName.FetchItem()}, msgC)
	}()
	headerSep := []byte("---\n")
	buf := &bytes.Buffer{}
	bodyBuf := &bytes.Buffer{}

	bodyHasher, err := blake2b.New256(nil)
	if err != nil {
		return err
	}

	for msg := range msgC {
		buf.Reset()
		bodyBuf.Reset()
		bodyHasher.Reset()

		name, err := fn(xof, key[:], msg.Envelope.MessageId)
		if err != nil {
			return fmt.Errorf("fn: %w", err)
		}

		body := msg.GetBody(secName)
		_, err = io.Copy(bodyBuf, io.TeeReader(body, bodyHasher))
		if err != nil {
			return fmt.Errorf("hash body")
		}

		from := ""
		if len(msg.Envelope.From) > 0 {
			f := msg.Envelope.From[0]
			if len(f.PersonalName) > 0 {
				from = fmt.Sprintf("%s <%s@%s>", f.PersonalName, f.MailboxName, f.HostName)
			} else {
				from = fmt.Sprintf("<%s@%s>", f.MailboxName, f.HostName)
			}
		}

		h := Header{
			Key:       name,
			MessageID: msg.Envelope.MessageId,
			Date:      msg.Envelope.Date.Format(time.RFC3339Nano),
			Folder:    mi.Name,
			Subject:   msg.Envelope.Subject,
			From:      from,
			Size:      strconv.FormatInt(int64(bodyBuf.Len()), 10),
			Hash:      bodyHasher.Sum(nil),
		}
		e := json.NewEncoder(buf)
		e.SetEscapeHTML(false)
		err = e.Encode(h)
		if err != nil {
			return fmt.Errorf("marshal header: %w", err)
		}
		buf.Write(headerSep)
		_, err = io.Copy(buf, bodyBuf)
		if err != nil {
			return fmt.Errorf("body read: %w", err)
		}

		fn := filepath.Join(w.Store, name)
		err = os.WriteFile(fn, buf.Bytes(), 0600)
		if err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-fetchErr:
		if err != nil {
			return err
		}
	}
	w.log("\tdone")

	return nil
}

type Header struct {
	Key       string
	MessageID string
	InReplyTo string // Parent MessageID.
	Date      string
	Folder    string
	Subject   string
	From      string
	Size      string // Length of Body in bytes.
	Hash      []byte // blake2b of Body.
}

func fn(xof blake2b.XOF, key []byte, msgID string) (string, error) {
	xof.Reset()
	xof.Write([]byte(msgID))
	n, err := xof.Read(key)
	if err != nil {
		return "", fmt.Errorf("xof read: %w", err)
	}
	if n != len(key) {
		return "", fmt.Errorf("xof invalid read %d: %w", n, err)
	}
	name := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(key)
	return name, nil
}
