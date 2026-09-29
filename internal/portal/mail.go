package portal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

const approvalMailSubject = "wanctl：你的访问申请已通过 / Your wanctl access request was approved"

type mailSender interface {
	Send(to, subject, body string) error
}

type smtpSender struct {
	addr, user, password, from string
}

func (s *Server) mailEnabled() bool { return s.mail != nil }

func approvalMailBody(login, origin string) string {
	return fmt.Sprintf("你好，%s：\n\n你在 %s 的访问申请已通过。\n请登录：%s/\n\n欢迎使用 wanctl。\n\nHello %s,\n\nYour access request on %s was approved.\nSign in: %s/\n\nWelcome to wanctl.\n", login, origin, origin, login, origin, origin)
}

func mailMessage(from, to *mail.Address, subject, body string) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n",
		from.String(), to.String(), mime.QEncoding.Encode("UTF-8", subject), time.Now().Format(time.RFC1123Z),
		rand.Text(), from.Address[strings.LastIndex(from.Address, "@")+1:])
	enc := quotedprintable.NewWriter(&out)
	_, _ = enc.Write([]byte(body))
	_ = enc.Close()
	return out.Bytes()
}

func (s *smtpSender) Send(to, subject, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	from, err := mail.ParseAddress(s.from)
	if err != nil {
		return fmt.Errorf("invalid mail sender")
	}
	recipient, err := mail.ParseAddress(to)
	if err != nil || recipient.Address != to || strings.ContainsAny(to, "\r\n") {
		return fmt.Errorf("invalid mail recipient")
	}
	host, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		return fmt.Errorf("invalid SMTP address")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	return s.sendConn(conn, host, port, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}, from, recipient, subject, body)
}

func (s *smtpSender) sendConn(conn net.Conn, host, port string, config *tls.Config, from, to *mail.Address, subject, body string) error {
	if port == "465" {
		conn = tls.Client(conn, config)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()
	if port != "465" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("SMTP server does not offer STARTTLS")
		}
		if err := client.StartTLS(config); err != nil {
			return err
		}
	}
	if err := client.Auth(smtp.PlainAuth("", s.user, s.password, host)); err != nil {
		return err
	}
	if err := client.Mail(from.Address); err != nil {
		return err
	}
	if err := client.Rcpt(to.Address); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(mailMessage(from, to, subject, body)); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// SMTP reply text may echo mailbox addresses; retain the status, not the text.
func mailError(err error, recipient string) string {
	var reply *textproto.Error
	if errors.As(err, &reply) {
		return fmt.Sprintf("SMTP status %d", reply.Code)
	}
	return strings.ReplaceAll(err.Error(), recipient, "[recipient]")
}
