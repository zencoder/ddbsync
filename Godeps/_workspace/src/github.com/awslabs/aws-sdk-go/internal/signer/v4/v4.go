package v4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/awslabs/aws-sdk-go/aws"
)

const (
	authHeaderPrefix = "AWS4-HMAC-SHA256"
	timeFormat       = "20060102T150405Z"
	shortTimeFormat  = "20060102"
)

var ignoredHeaders = map[string]bool{
	"Authorization":  true,
	"Content-Type":   true,
	"Content-Length": true,
	"User-Agent":     true,
}

type signer struct {
	Request         *http.Request
	Time            time.Time
	ExpireTime      time.Duration
	ServiceName     string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Query           url.Values
	Body            io.ReadSeeker
	Debug           uint
	Logger          io.Writer

	isPresign          bool
	formattedTime      string
	formattedShortTime string

	signedHeaders    string
	canonicalHeaders string
	canonicalString  string
	credentialString string
	stringToSign     string
	signature        string
	authorization    string
}

// Sign requests with signature version 4.
func Sign(req *aws.Request) {
	creds, err := req.Service.Config.Credentials.Credentials()
	if err != nil {
		req.Error = err
		return
	}

	s := signer{
		Request:         req.HTTPRequest,
		Time:            req.Time,
		ExpireTime:      req.ExpireTime,
		Query:           req.HTTPRequest.URL.Query(),
		Body:            req.Body,
		ServiceName:     req.Service.ServiceName,
		Region:          req.Service.Config.Region,
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
		Debug:           req.Service.Config.LogLevel,
		Logger:          req.Service.Config.Logger,
	}
	s.sign()
	return
}

func (v4 *signer) sign() {
	if v4.ExpireTime != 0 {
		v4.isPresign = true
	}

	if v4.isPresign {
		v4.Query.Set("X-Amz-Algorithm", authHeaderPrefix)
		if v4.SessionToken != "" {
			v4.Query.Set("X-Amz-Security-Token", v4.SessionToken)
		} else {
			v4.Query.Del("X-Amz-Security-Token")
		}
	} else if v4.SessionToken != "" {
		v4.Request.Header.Set("X-Amz-Security-Token", v4.SessionToken)
	}

	v4.build()

	if v4.Debug > 0 {
		out := v4.Logger
		fmt.Fprintf(out, "---[ CANONICAL STRING  ]-----------------------------\n")
		fmt.Fprintln(out, v4.canonicalString)
		fmt.Fprintf(out, "---[ STRING TO SIGN ]--------------------------------\n")
		fmt.Fprintln(out, v4.stringToSign)
		fmt.Fprintf(out, "---[ SIGNED URL ]--------------------------------\n")
		fmt.Fprintln(out, v4.Request.URL)
		fmt.Fprintf(out, "-----------------------------------------------------\n")
	}
}

func (v4 *signer) build() {
	v4.buildTime()             // no depends
	v4.buildCredentialString() // no depends
	if v4.isPresign {
		v4.buildQuery() // no depends
	}
	v4.buildCanonicalHeaders() // depends on cred string
	v4.buildCanonicalString()  // depends on canon headers / signed headers
	v4.buildStringToSign()     // depends on canon string
	v4.buildSignature()        // depends on string to sign

	if v4.isPresign {
		v4.Request.URL.RawQuery += "&X-Amz-Signature=" + v4.signature
	} else {
		parts := []string{
			authHeaderPrefix + " Credential=" + v4.AccessKeyID + "/" + v4.credentialString,
			"SignedHeaders=" + v4.signedHeaders,
			"Signature=" + v4.signature,
		}
		v4.Request.Header.Set("Authorization", strings.Join(parts, ", "))
	}
}

func (v4 *signer) buildTime() {
	v4.formattedTime = v4.Time.UTC().Format(timeFormat)
	v4.formattedShortTime = v4.Time.UTC().Format(shortTimeFormat)

	if v4.isPresign {
		duration := int64(v4.ExpireTime / time.Second)
		v4.Query.Set("X-Amz-Date", v4.formattedTime)
		v4.Query.Set("X-Amz-Expires", strconv.FormatInt(duration, 10))
	} else {
		v4.Request.Header.Set("X-Amz-Date", v4.formattedTime)
	}
}

func (v4 *signer) buildCredentialString() {
	v4.credentialString = strings.Join([]string{
		v4.formattedShortTime,
		v4.Region,
		v4.ServiceName,
		"aws4_request",
	}, "/")

	if v4.isPresign {
		v4.Query.Set("X-Amz-Credential", v4.AccessKeyID+"/"+v4.credentialString)
	}
}

func (v4 *signer) buildQuery() {
	for k, h := range v4.Request.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Amz-") {
			continue // never hoist x-amz-* headers, they must be signed
		}
		if _, ok := ignoredHeaders[http.CanonicalHeaderKey(k)]; ok {
			continue // never hoist ignored headers
		}

		v4.Request.Header.Del(k)
		v4.Query.Del(k)
		for _, v := range h {
			v4.Query.Add(k, v)
		}
	}
}

func (v4 *signer) buildCanonicalHeaders() {
	headers := make([]string, 0)
	headers = append(headers, "host")
	for k, _ := range v4.Request.Header {
		if _, ok := ignoredHeaders[http.CanonicalHeaderKey(k)]; ok {
			continue // ignored header
		}
		headers = append(headers, strings.ToLower(k))
	}
	sort.Strings(headers)

	v4.signedHeaders = strings.Join(headers, ";")

	if v4.isPresign {
		v4.Query.Set("X-Amz-SignedHeaders", v4.signedHeaders)
	}

	headerValues := make([]string, len(headers))
	for i, k := range headers {
		if k == "host" {
			headerValues[i] = "host:" + v4.Request.URL.Host
		} else {
			headerValues[i] = k + ":" +
				strings.Join(v4.Request.Header[http.CanonicalHeaderKey(k)], ",")
		}
	}

	v4.canonicalHeaders = strings.Join(headerValues, "\n")
}

func (v4 *signer) buildCanonicalString() {
	v4.Request.URL.RawQuery = v4.Query.Encode()
	uri := v4.Request.URL.Opaque
	if uri != "" {
		uri = "/" + strings.Join(strings.Split(uri, "/")[3:], "/")
	} else {
		uri = v4.Request.URL.Path
	}
	if uri == "" {
		uri = "/"
	}

	v4.canonicalString = strings.Join([]string{
		v4.Request.Method,
		uri,
		v4.Request.URL.RawQuery,
		v4.canonicalHeaders + "\n",
		v4.signedHeaders,
		v4.bodyDigest(),
	}, "\n")
}

func (v4 *signer) buildStringToSign() {
	v4.stringToSign = strings.Join([]string{
		authHeaderPrefix,
		v4.formattedTime,
		v4.credentialString,
		hex.EncodeToString(makeSha256([]byte(v4.canonicalString))),
	}, "\n")
}

func (v4 *signer) buildSignature() {
	secret := v4.SecretAccessKey
	date := makeHmac([]byte("AWS4"+secret), []byte(v4.formattedShortTime))
	region := makeHmac(date, []byte(v4.Region))
	service := makeHmac(region, []byte(v4.ServiceName))
	credentials := makeHmac(service, []byte("aws4_request"))
	signature := makeHmac(credentials, []byte(v4.stringToSign))
	v4.signature = hex.EncodeToString(signature)
}

func (v4 *signer) bodyDigest() string {
	hash := v4.Request.Header.Get("X-Amz-Content-Sha256")
	if hash == "" {
		if v4.Body == nil {
			if v4.ServiceName == "s3" {
				hash = "UNSIGNED-PAYLOAD"
			} else {
				hash = hex.EncodeToString(makeSha256([]byte{}))
			}
		} else {
			hash = hex.EncodeToString(makeSha256Reader(v4.Body))
		}
		v4.Request.Header.Add("X-Amz-Content-Sha256", hash)
	}
	return hash
}

func makeHmac(key []byte, data []byte) []byte {
	hash := hmac.New(sha256.New, key)
	hash.Write(data)
	return hash.Sum(nil)
}

func makeSha256(data []byte) []byte {
	hash := sha256.New()
	hash.Write(data)
	return hash.Sum(nil)
}

func makeSha256Reader(reader io.ReadSeeker) []byte {
	packet := make([]byte, 4096)
	hash := sha256.New()

	reader.Seek(0, 0)
	for {
		n, err := reader.Read(packet)
		if n > 0 {
			hash.Write(packet[0:n])
		}
		if err == io.EOF || n == 0 {
			break
		}
	}
	reader.Seek(0, 0)

	return hash.Sum(nil)
}