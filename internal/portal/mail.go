package portal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

const approvalMailSubject = "wanctl 访问申请已通过 · Access approved"

// mailContent is one message in both forms a mail client may show: the HTML
// where it renders HTML, the text where it does not. Notification previews
// often read the text part even in clients that render the HTML.
type mailContent struct {
	subject, text, html string
}

type mailSender interface {
	Send(to string, m mailContent) error
}

type smtpSender struct {
	addr, user, password, from string
}

func (s *Server) mailEnabled() bool { return s.mail != nil }

// The approval mail is a file under web/ for the reasons the auth pages are
// (see pages.go): tools/portalpreview renders the exact bytes that are sent,
// and html/template escapes the GitHub login by context. handleAsset never
// serves .html, so the template is not reachable as a URL.
var approvalMailPage = template.Must(template.ParseFS(assets, "web/mail-approved.html"))

// approvalMail tells an applicant they are in. It says three things: they
// were approved, where to sign in (the one link, once), and why this mail
// reached them. Both languages, because the address is all we know of them.
func approvalMail(login, origin string) (mailContent, error) {
	host := origin
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		host = u.Host
	}
	link := origin + "/"
	var page bytes.Buffer
	if err := approvalMailPage.Execute(&page, struct{ Login, Host, Link string }{login, host, link}); err != nil {
		return mailContent{}, err
	}
	text := fmt.Sprintf("访问申请已通过 · Your access request was approved\n\n"+
		"现在可以登录 %[2]s，接入你的第一台设备。\n"+
		"You can now sign in to %[2]s and connect your first device.\n\n"+
		"登录 · Sign in: %[3]s\n\n"+
		"你收到这封邮件，是因为 GitHub 账号 %[1]s 申请了访问。\n"+
		"You received this because the GitHub account %[1]s asked for access.\n", login, host, link)
	return mailContent{subject: approvalMailSubject, text: text, html: page.String()}, nil
}

// mailMessage is multipart/alternative, text first: a client shows the last
// part it can render.
func mailMessage(from, to *mail.Address, m mailContent) []byte {
	var body bytes.Buffer
	parts := multipart.NewWriter(&body)
	for _, part := range []struct{ contentType, content string }{
		{"text/plain; charset=UTF-8", m.text},
		{"text/html; charset=UTF-8", m.html},
	} {
		w, _ := parts.CreatePart(textproto.MIMEHeader{"Content-Type": {part.contentType}, "Content-Transfer-Encoding": {"quoted-printable"}})
		enc := quotedprintable.NewWriter(w)
		_, _ = enc.Write([]byte(part.content))
		_ = enc.Close()
	}
	_ = parts.Close()
	var out bytes.Buffer
	fmt.Fprintf(&out, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\nContent-Type: %s\r\n\r\n",
		from.String(), to.String(), mime.QEncoding.Encode("UTF-8", m.subject), time.Now().Format(time.RFC1123Z),
		rand.Text(), from.Address[strings.LastIndex(from.Address, "@")+1:],
		mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": parts.Boundary()}))
	out.Write(body.Bytes())
	return out.Bytes()
}

func (s *smtpSender) Send(to string, m mailContent) error {
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
	return s.sendConn(conn, host, port, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}, from, recipient, m)
}

func (s *smtpSender) sendConn(conn net.Conn, host, port string, config *tls.Config, from, to *mail.Address, m mailContent) error {
	if port == "465" {
		conn = tls.Client(conn, config)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return smtpStep("greeting", err)
	}
	defer client.Close()
	if port != "465" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("SMTP server does not offer STARTTLS")
		}
		if err := client.StartTLS(config); err != nil {
			return smtpStep("starttls", err)
		}
	}
	if err := client.Auth(smtp.PlainAuth("", s.user, s.password, host)); err != nil {
		return smtpStep("auth", err)
	}
	if err := client.Mail(from.Address); err != nil {
		return smtpStep("mail from", err)
	}
	if err := client.Rcpt(to.Address); err != nil {
		return smtpStep("rcpt to", err)
	}
	writer, err := client.Data()
	if err != nil {
		return smtpStep("data", err)
	}
	if _, err := writer.Write(mailMessage(from, to, m)); err != nil {
		return smtpStep("data", err)
	}
	if err := writer.Close(); err != nil {
		return smtpStep("data", err)
	}
	// The server accepted the message when DATA closed with 250. A server that
	// hangs up on QUIT has still taken it, and reporting that as a failure
	// would withdraw a confirmation link that is already in someone's inbox.
	_ = client.Quit()
	return nil
}

// smtpStepError names the SMTP step that failed: "EOF" alone does not say
// whether the server hung up on the greeting or after accepting the message.
type smtpStepError struct {
	step string
	err  error
}

func (e *smtpStepError) Error() string { return "smtp " + e.step + ": " + e.err.Error() }
func (e *smtpStepError) Unwrap() error { return e.err }

func smtpStep(step string, err error) error {
	if err == nil {
		return nil
	}
	return &smtpStepError{step, err}
}

// SMTP reply text may echo mailbox addresses; retain the status, not the text.
func mailError(err error, recipient string) string {
	prefix := ""
	var step *smtpStepError
	if errors.As(err, &step) {
		prefix = "smtp " + step.step + ": "
	}
	var reply *textproto.Error
	if errors.As(err, &reply) {
		return fmt.Sprintf("%sSMTP status %d", prefix, reply.Code)
	}
	return strings.ReplaceAll(err.Error(), recipient, "[recipient]")
}
