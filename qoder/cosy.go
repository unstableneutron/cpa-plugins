package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const publicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var keyOnce sync.Once
var publicKey *rsa.PublicKey
var publicKeyErr error

func getPublicKey() (*rsa.PublicKey, error) {
	keyOnce.Do(func() {
		block, _ := pem.Decode([]byte(publicKeyPEM))
		if block == nil {
			publicKeyErr = fmt.Errorf("decode COSY public key")
			return
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			publicKeyErr = err
			return
		}
		var ok bool
		publicKey, ok = parsed.(*rsa.PublicKey)
		if !ok {
			publicKeyErr = fmt.Errorf("COSY key is not RSA")
		}
	})
	return publicKey, publicKeyErr
}
func encodeBody(plain []byte) string {
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	a := n / 3
	reordered := std[n-a:] + std[a:n-a] + std[:a]
	const from = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="
	const to = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!$"
	return strings.Map(func(r rune) rune {
		i := strings.IndexRune(from, r)
		if i < 0 {
			return r
		}
		return rune(to[i])
	}, reordered)
}
func pkcs7(data []byte, n int) []byte {
	padding := n - len(data)%n
	return append(data, bytesRepeat(byte(padding), padding)...)
}
func bytesRepeat(v byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = v
	}
	return b
}
func cosyHeaders(body []byte, requestURL string, s tokenStorage) (http.Header, error) {
	if s.UserID == "" || s.Token == "" {
		return nil, fmt.Errorf("COSY user id and token are required")
	}
	aesKey := randomUUID()[:16]
	block, err := aes.NewCipher([]byte(aesKey))
	if err != nil {
		return nil, err
	}
	info, _ := json.Marshal(map[string]string{"uid": s.UserID, "security_oauth_token": s.Token, "name": s.Name, "aid": "", "email": s.Email})
	encrypted := make([]byte, len(pkcs7(info, aes.BlockSize)))
	cipher.NewCBCEncrypter(block, []byte(aesKey)).CryptBlocks(encrypted, pkcs7(info, aes.BlockSize))
	info64 := base64.StdEncoding.EncodeToString(encrypted)
	pub, err := getPublicKey()
	if err != nil {
		return nil, err
	}
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(aesKey))
	if err != nil {
		return nil, err
	}
	cosyKey := base64.StdEncoding.EncodeToString(wrapped)
	parsed, err := url.Parse(requestURL)
	if err != nil {
		return nil, err
	}
	sigPath := strings.TrimPrefix(parsed.Path, "/algo")
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	requestID := randomUUID()
	payload, _ := json.Marshal(map[string]string{"version": "v1", "requestId": requestID, "info": info64, "cosyVersion": "1.0.0", "ideVersion": ""})
	payload64 := base64.StdEncoding.EncodeToString(payload)
	sig := fmt.Sprintf("%x", md5.Sum([]byte(payload64+"\n"+cosyKey+"\n"+timestamp+"\n"+string(body)+"\n"+sigPath)))
	machine := s.MachineID
	if machine == "" {
		machine = randomUUID()
	}
	h := http.Header{"Authorization": {"Bearer COSY." + payload64 + "." + sig}, "Cosy-Key": {cosyKey}, "Cosy-User": {s.UserID}, "Cosy-Date": {timestamp}, "Cosy-Version": {"1.0.0"}, "Cosy-Machineid": {machine}, "Cosy-Machinetoken": {machine}, "Cosy-Machinetype": {"5"}, "Cosy-Machineos": {"x86_64_windows"}, "Cosy-Clienttype": {"5"}, "Cosy-Clientip": {"127.0.0.1"}, "Cosy-Bodyhash": {fmt.Sprintf("%x", md5.Sum(body))}, "Cosy-Bodylength": {strconv.Itoa(len(body))}, "Cosy-Sigpath": {sigPath}, "Cosy-Data-Policy": {"disagree"}, "Cosy-Organization-Id": {""}, "Cosy-Organization-Tags": {""}, "Login-Version": {"v2"}, "X-Request-Id": {randomUUID()}}
	return h, nil
}
func pkce() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}
