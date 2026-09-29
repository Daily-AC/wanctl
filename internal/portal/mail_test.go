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
	body := approvalMailBody("octocat", "https://portal.example")
	raw := mailMessage(from, to, approvalMailSubject, body)
	message, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := (&mime.WordDecoder{}).DecodeHeader(message.Header.Get("Subject"))
	if err != nil || subject != approvalMailSubject {
		t.Fatalf("subject = %q, %v", subject, err)
	}
	for key, want := range map[string]string{"From": from.String(), "To": to.String(), "MIME-Version": "1.0", "Content-Type": "text/plain; charset=UTF-8", "Content-Transfer-Encoding": "quoted-printable"} {
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
	decoded, err := io.ReadAll(quotedprintable.NewReader(message.Body))
	if err != nil || strings.ReplaceAll(string(decoded), "\r\n", "\n") != body {
		t.Fatalf("body = %q, %v", decoded, err)
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("oversized line: %d", len(line))
		}
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
					if !strings.Contains(string(data), "Content-Transfer-Encoding: quoted-printable") {
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
			err := sender.sendConn(client, "smtp.example", port, clientTLS, &mail.Address{Address: "wanctl@example.com"}, &mail.Address{Address: "recipient@example.com"}, approvalMailSubject, "你好\nHello\n")
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
	err := sender.sendConn(client, "smtp.example", "587", &tls.Config{ServerName: "smtp.example", MinVersion: tls.VersionTLS12}, &mail.Address{Address: "wanctl@example.com"}, &mail.Address{Address: "recipient@example.com"}, "subject", "body")
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
