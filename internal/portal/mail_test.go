package portal

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func TestMailMessage(t *testing.T) {
	from := &mail.Address{Name: "wanctl", Address: "wanctl@example.com"}
	to := &mail.Address{Address: "recipient@example.com"}
	content, err := approvalMail("octocat", "https://portal.example")
	if err != nil {
		t.Fatal(err)
	}
	raw := mailMessage(from, to, content)
	message, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := (&mime.WordDecoder{}).DecodeHeader(message.Header.Get("Subject"))
	if err != nil || subject != approvalMailSubject {
		t.Fatalf("subject = %q, %v", subject, err)
	}
	for key, want := range map[string]string{"From": from.String(), "To": to.String(), "MIME-Version": "1.0"} {
		if message.Header.Get(key) != want {
			t.Errorf("%s = %q", key, message.Header.Get(key))
		}
	}
	if _, err := mail.ParseDate(message.Header.Get("Date")); err != nil {
		t.Fatal(err)
	}
	if id := message.Header.Get("Message-ID"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.com>") {
		t.Fatalf("message id = %q", id)
	}
	mediaType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" || params["boundary"] == "" {
		t.Fatalf("content type = %q, %v", message.Header.Get("Content-Type"), err)
	}
	// Text first: a client shows the last part it can render. NextRawPart,
	// because NextPart would decode quoted-printable and hide the header.
	parts := multipart.NewReader(message.Body, params["boundary"])
	for _, want := range []struct{ contentType, body string }{
		{"text/plain; charset=UTF-8", content.text},
		{"text/html; charset=UTF-8", content.html},
	} {
		part, err := parts.NextRawPart()
		if err != nil {
			t.Fatal(err)
		}
		if got := part.Header.Get("Content-Type"); got != want.contentType {
			t.Errorf("part type = %q, want %q", got, want.contentType)
		}
		if got := part.Header.Get("Content-Transfer-Encoding"); got != "quoted-printable" {
			t.Errorf("%s encoding = %q", want.contentType, got)
		}
		decoded, err := io.ReadAll(quotedprintable.NewReader(part))
		if err != nil || strings.ReplaceAll(string(decoded), "\r\n", "\n") != want.body {
			t.Fatalf("%s body = %q, %v", want.contentType, decoded, err)
		}
	}
	if _, err := parts.NextRawPart(); err != io.EOF {
		t.Fatalf("after two parts: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("oversized line: %d", len(line))
		}
	}
}

// The applicant needs three things from this mail: that they are in, where to
// sign in, and why it reached them. The link is there once, the internal
// notes in the template are not sent, and the GitHub login — the one value
// from outside — cannot become markup.
func TestApprovalMailContent(t *testing.T) {
	m, err := approvalMail("octocat", "https://portal.example")
	if err != nil {
		t.Fatal(err)
	}
	if m.subject != approvalMailSubject {
		t.Errorf("subject = %q", m.subject)
	}
	if n := strings.Count(m.text, "https://"); n != 1 || !strings.Contains(m.text, "https://portal.example/") {
		t.Errorf("text has %d links:\n%s", n, m.text)
	}
	if n := strings.Count(m.html, "<a "); n != 1 || !strings.Contains(m.html, `href="https://portal.example/"`) {
		t.Errorf("html has %d links, want the one button to https://portal.example/", n)
	}
	for _, want := range []string{"octocat", "portal.example"} {
		if !strings.Contains(m.text, want) || !strings.Contains(m.html, want) {
			t.Errorf("%q missing from a part", want)
		}
	}
	for _, unwanted := range []string{"<!--", "<script", "<style", "<img", "<svg", "<link"} {
		if strings.Contains(m.html, unwanted) {
			t.Errorf("html carries %s", unwanted)
		}
	}

	hostile := "<script>alert(1)</script>"
	m, err = approvalMail(hostile, "https://portal.example")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.html, hostile) || !strings.Contains(m.html, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("login reached the html unescaped")
	}
}

func smtpTestTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "smtp.example"}, DNSNames: []string{"smtp.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return &tls.Config{ServerName: "smtp.example", RootCAs: roots, MinVersion: tls.VersionTLS12}, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
}

// Exercise the SMTP protocol and TLS over a pipe: no listening ports required.
func TestSMTPEncryptedDelivery(t *testing.T) {
	for _, port := range []string{"465", "587", "2525"} {
		t.Run(port, func(t *testing.T) {
			clientTLS, serverTLS := smtpTestTLS(t)
			client, server := net.Pipe()
			defer client.Close()
			deadline := time.Now().Add(3 * time.Second)
			client.SetDeadline(deadline)
			server.SetDeadline(deadline)
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				conn := server
				if port == "465" {
					conn = tls.Server(conn, serverTLS)
				}
				rw := textproto.NewConn(conn)
				reply := func(text string) error { return rw.PrintfLine("%s", text) }
				expect := func(prefix string) error {
					line, err := rw.ReadLine()
					if err != nil {
						return err
					}
					if !strings.HasPrefix(line, prefix) {
						return fmt.Errorf("got %q, want %q", line, prefix)
					}
					return nil
				}
				run := func() error {
					if err := reply("220 smtp.example ready"); err != nil {
						return err
					}
					if err := expect("EHLO "); err != nil {
						return err
					}
					if port != "465" {
						if err := reply("250-smtp.example\r\n250 STARTTLS"); err != nil {
							return err
						}
						if err := expect("STARTTLS"); err != nil {
							return err
						}
						if err := reply("220 ready for TLS"); err != nil {
							return err
						}
						conn = tls.Server(server, serverTLS)
						rw = textproto.NewConn(conn)
						if err := expect("EHLO "); err != nil {
							return err
						}
					}
					if err := reply("250-smtp.example\r\n250 AUTH PLAIN"); err != nil {
						return err
					}
					if err := expect("AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00user\x00password"))); err != nil {
						return err
					}
					if err := reply("235 authenticated"); err != nil {
						return err
					}
					if err := expect("MAIL FROM:<wanctl@example.com>"); err != nil {
						return err
					}
					if err := reply("250 sender ok"); err != nil {
						return err
					}
					if err := expect("RCPT TO:<recipient@example.com>"); err != nil {
						return err
					}
					if err := reply("250 recipient ok"); err != nil {
						return err
					}
					if err := expect("DATA"); err != nil {
						return err
					}
					if err := reply("354 send message"); err != nil {
						return err
					}
					data, err := rw.ReadDotBytes()
					if err != nil {
						return err
					}
					if !strings.Contains(string(data), "multipart/alternative") || !strings.Contains(string(data), "Content-Transfer-Encoding: quoted-printable") {
						return fmt.Errorf("missing MIME message")
					}
					if err := reply("250 queued"); err != nil {
						return err
					}
					if err := expect("QUIT"); err != nil {
						return err
					}
					if err := reply("221 bye"); err != nil {
						return err
					}
					_, err = io.Copy(io.Discard, conn)
					return err
				}
				done <- run()
			}()
			sender := &smtpSender{user: "user", password: "password"}
			err := sender.sendConn(client, "smtp.example", port, clientTLS, &mail.Address{Address: "wanctl@example.com"}, &mail.Address{Address: "recipient@example.com"}, mailContent{subject: approvalMailSubject, text: "你好\nHello\n", html: "<p>你好</p>\n"})
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSMTPRefusesPlaintext(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	deadline := time.Now().Add(time.Second)
	client.SetDeadline(deadline)
	server.SetDeadline(deadline)
	done := make(chan string, 1)
	go func() {
		defer server.Close()
		fmt.Fprint(server, "220 ready\r\n")
		reader := bufio.NewReader(server)
		reader.ReadString('\n')
		fmt.Fprint(server, "250-smtp.example\r\n250 AUTH PLAIN\r\n")
		rest, _ := io.ReadAll(reader)
		done <- string(rest)
	}()
	sender := &smtpSender{user: "user", password: "password"}
	err := sender.sendConn(client, "smtp.example", "587", &tls.Config{ServerName: "smtp.example", MinVersion: tls.VersionTLS12}, &mail.Address{Address: "wanctl@example.com"}, &mail.Address{Address: "recipient@example.com"}, mailContent{subject: "subject", text: "body", html: "<p>body</p>"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("error = %v", err)
	}
	if rest := <-done; strings.Contains(rest, "AUTH") || strings.Contains(rest, "MAIL") {
		t.Fatalf("sent over plaintext: %q", rest)
	}
}

func TestMailErrorsDoNotExposeRecipient(t *testing.T) {
	for _, err := range []error{
		&textproto.Error{Code: 550, Msg: "Rejected <RECIPIENT@EXAMPLE.COM>"},
		fmt.Errorf("SMTP failed: %w", &textproto.Error{Code: 550, Msg: "Rejected recipient@example.com"}),
		fmt.Errorf("delivery to recipient@example.com failed"),
	} {
		got := mailError(err, "recipient@example.com")
		if strings.Contains(strings.ToLower(got), "recipient@example.com") || got == "" {
			t.Fatalf("unsafe error: %q", got)
		}
	}
}
