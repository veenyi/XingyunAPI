package devcloud

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// signer decodes the base64 PEM private key from ssh-config.
func (cfg *SSHConfig) signer() (ssh.Signer, error) {
	if cfg.PrivateKey == "" {
		return nil, fmt.Errorf("ssh-config: empty private key")
	}
	raw, err := base64.StdEncoding.DecodeString(cfg.PrivateKey)
	if err != nil {
		// Some deployments return the PEM directly instead of base64.
		raw = []byte(cfg.PrivateKey)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("ssh-config: no PEM block in private key")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		return ssh.NewSignerFromKey(key)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		return ssh.NewSignerFromKey(key)
	default:
		return ssh.ParsePrivateKey(raw)
	}
}

// DialSSH fetches the project's ssh-config and opens an SSH connection.
// Host keys are not verified (the gateway rotates per-pod; the config itself
// came from an authenticated API).
func (c *Client) DialSSH(projectID int) (*ssh.Client, *SSHConfig, error) {
	cfg, err := c.SSHConfig(projectID)
	if err != nil {
		return nil, nil, err
	}
	signer, err := cfg.signer()
	if err != nil {
		return nil, nil, err
	}
	port := cfg.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", port))
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	})
	if err != nil {
		return nil, cfg, err
	}
	return client, cfg, nil
}
