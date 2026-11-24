// Spike 07: how does an S3 store treat presigned uploads and downloads?
//
// Runs a table of cases (see docs/research/07-uploads.md, section 11) and prints
// the HTTP status, S3 error code and whether the object exists afterwards.
//
// Default: an in-process gofakes3 server (no setup, no ports to configure).
// Real store: set
//
//	S3_ENDPOINT    e.g. http://localhost:3900 (empty string = AWS)
//	S3_REGION      e.g. garage or us-east-1
//	S3_BUCKET      an existing bucket
//	S3_PATH_STYLE  1 for Garage
//	AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

var (
	ctx     = context.Background()
	bucket  string
	client  *s3.Client
	presign *s3.PresignClient
	keys    []string // objects to delete at the end
)

func main() {
	endpoint, real := os.LookupEnv("S3_ENDPOINT")
	region := env("S3_REGION", "us-east-1")
	bucket = env("S3_BUCKET", "spike07")
	pathStyle := os.Getenv("S3_PATH_STYLE") == "1"
	creds := aws.Credentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY")}

	if !real {
		srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
		defer srv.Close()
		endpoint, pathStyle = srv.URL, true
		creds = aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}
		fmt.Println("store: gofakes3 (in-process)")
	} else {
		fmt.Printf("store: %q region=%s bucket=%s\n", endpoint, region, bucket)
	}

	client = s3.New(s3.Options{
		Region:                     region,
		Credentials:                credentials.StaticCredentialsProvider{Value: creds},
		UsePathStyle:               pathStyle,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	if !real {
		_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket})
		check(err)
	}
	presign = s3.NewPresignClient(client)

	run := time.Now().Format("150405")
	k := func(name string) string { key := "spike07/" + run + "/" + name; keys = append(keys, key); return key }

	good := randBytes(1 << 20)
	bad := append([]byte{}, good...)
	bad[0] ^= 0xff
	other := randBytes(1 << 20) // same length, different bytes (another device's ciphertext)

	probe := signPut(k("probe"), good, 15*time.Minute, nil)
	fmt.Println("signed headers:", headerNames(probe.SignedHeader))
	fmt.Println()

	// 1-4: checksum enforcement.
	report("1  correct body", k("c1"), put(signPut(k("c1"), good, 15*time.Minute, nil), good, nil))
	report("2  tampered body", k("c2"), put(signPut(k("c2"), good, 15*time.Minute, nil), bad, nil))
	report("3  checksum header omitted", k("c3"), put(signPut(k("c3"), good, 15*time.Minute, nil), good, dropChecksum))
	report("4  different checksum header", k("c4"), put(signPut(k("c4"), good, 15*time.Minute, nil), good,
		func(h http.Header) { h.Set("X-Amz-Checksum-Sha256", b64sum(bad)) }))

	// 5: tampered upload over an existing object must not replace it.
	k5 := k("c5")
	put(signPut(k5, good, 15*time.Minute, nil), good, nil)
	r5 := put(signPut(k5, good, 15*time.Minute, nil), bad, nil)
	report("5  tampered over existing", k5, r5)
	fmt.Printf("     existing object intact: %v\n", sameAs(k5, good))

	// 6: body length differs from signed Content-Length.
	report("6  body 1 byte shorter", k("c6"), put(signPut(k("c6"), good, 15*time.Minute, nil), good[:len(good)-1], nil))

	// 7: ChecksumAlgorithm also set (adds X-Amz-Sdk-Checksum-Algorithm query param).
	report("7  ChecksumAlgorithm also set", k("c7"), put(signPut(k("c7"), good, 15*time.Minute,
		func(in *s3.PutObjectInput) { in.ChecksumAlgorithm = types.ChecksumAlgorithmSha256 }), good, nil))

	// 8: If-None-Match: * on a second write.
	k8 := k("c8")
	ifNone := func(in *s3.PutObjectInput) { in.IfNoneMatch = aws.String("*") }
	put(signPut(k8, good, 15*time.Minute, ifNone), good, nil)
	report("8  If-None-Match, 2nd write", k8, put(signPut(k8, other, 15*time.Minute, ifNone), other, nil))
	fmt.Printf("     first write kept: %v\n", sameAs(k8, good))

	// 9: URL used after it expired.
	p9 := signPut(k("c9"), good, 2*time.Second, nil)
	time.Sleep(4 * time.Second)
	report("9  used after expiry (2 s)", k("c9"), put(p9, good, nil))

	// 10: expiry longer than 7 days.
	report("10 expiry 8 days", k("c10"), put(signPut(k("c10"), good, 8*24*time.Hour, nil), good, nil))

	// 11: signed for one host, sent to another (localhost vs 127.0.0.1).
	p11 := signPut(k("c11"), good, 15*time.Minute, nil)
	if u, _ := url.Parse(p11.URL); strings.HasPrefix(u.Host, "localhost") || strings.HasPrefix(u.Host, "127.0.0.1") {
		if strings.HasPrefix(u.Host, "localhost") {
			u.Host = strings.Replace(u.Host, "localhost", "127.0.0.1", 1)
		} else {
			u.Host = strings.Replace(u.Host, "127.0.0.1", "localhost", 1)
		}
		p11.URL = u.String()
		report("11 different host", k("c11"), put(p11, good, nil))
	} else {
		fmt.Println("11 different host                  skipped (not a local endpoint)")
	}

	// 12: presigned ranged GET; client sends the signed range, then a different one.
	k12 := k("c12")
	put(signPut(k12, good, 15*time.Minute, nil), good, nil)
	g, err := presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &k12, Range: aws.String("bytes=100-199")})
	check(err)
	st, body, _ := do(g.Method, g.URL, g.SignedHeader, nil, nil)
	fmt.Printf("%-34s HTTP %d, %d bytes, matches: %v\n", "12 GET signed range", st, len(body), bytes.Equal(body, good[100:200]))
	st, body, _ = do(g.Method, g.URL, g.SignedHeader, nil, func(h http.Header) { h.Set("Range", "bytes=0-999") })
	fmt.Printf("%-34s HTTP %d, %d bytes %s\n", "12 GET different range", st, len(body), errCode(body))

	// 13: two concurrent PUTs of different ciphertexts to the same key.
	k13 := k("c13")
	pa, pb := signPut(k13, good, 15*time.Minute, nil), signPut(k13, other, 15*time.Minute, nil)
	var wg sync.WaitGroup
	var ra, rb result
	wg.Add(2)
	go func() { defer wg.Done(); ra = put(pa, good, nil) }()
	go func() { defer wg.Done(); rb = put(pb, other, nil) }()
	wg.Wait()
	fmt.Printf("%-34s HTTP %d / %d; stored = A:%v B:%v\n", "13 concurrent PUTs", ra.status, rb.status, sameAs(k13, good), sameAs(k13, other))

	// 14: HEAD returns length, and checksum when asked.
	h, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: aws.String(keys[1]), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		fmt.Printf("%-34s error: %v\n", "14 HEAD", err)
	} else {
		fmt.Printf("%-34s length=%d checksum matches=%v\n", "14 HEAD (ChecksumMode enabled)", aws.ToInt64(h.ContentLength), aws.ToString(h.ChecksumSHA256) == b64sum(good))
	}

	if real {
		for _, key := range keys {
			client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: aws.String(key)})
		}
	}
}

type presigned struct {
	Method       string
	URL          string
	SignedHeader http.Header
}

type result struct {
	status int
	code   string
}

// signPut presigns a PUT for body, binding its SHA-256 and length into the signature.
func signPut(key string, body []byte, expires time.Duration, opt func(*s3.PutObjectInput)) presigned {
	in := &s3.PutObjectInput{
		Bucket:         &bucket,
		Key:            &key,
		ChecksumSHA256: aws.String(b64sum(body)),
		ContentLength:  aws.Int64(int64(len(body))),
	}
	if opt != nil {
		opt(in)
	}
	r, err := presign.PresignPutObject(ctx, in, s3.WithPresignExpires(expires))
	check(err)
	return presigned{r.Method, r.URL, r.SignedHeader}
}

func put(p presigned, body []byte, tweak func(http.Header)) result {
	st, msg, err := do(p.Method, p.URL, p.SignedHeader, body, tweak)
	if err != nil {
		return result{0, err.Error()}
	}
	return result{st, errCode(msg)}
}

// do sends exactly the headers the SDK signed (minus Host, which net/http sets), then applies tweak.
func do(method, u string, signed http.Header, body []byte, tweak func(http.Header)) (int, []byte, error) {
	req, _ := http.NewRequest(method, u, bytes.NewReader(body))
	for k, vs := range signed {
		if k == "Host" || k == "Content-Length" {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.ContentLength = int64(len(body))
	if tweak != nil {
		tweak(req.Header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

func report(name, key string, r result) {
	fmt.Printf("%-34s HTTP %d %-24s exists=%v\n", name, r.status, r.code, exists(key))
}

func dropChecksum(h http.Header) { h.Del("X-Amz-Checksum-Sha256") }

func exists(key string) bool {
	_, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &key})
	return err == nil
}

func sameAs(key string, want []byte) bool {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return false
	}
	defer out.Body.Close()
	got, _ := io.ReadAll(out.Body)
	return bytes.Equal(got, want)
}

var codeRE = regexp.MustCompile(`<Code>([^<]+)</Code>`)

func errCode(b []byte) string {
	if m := codeRE.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

func headerNames(h http.Header) string {
	var n []string
	for k := range h {
		n = append(n, k)
	}
	return strings.Join(n, ", ")
}

func b64sum(b []byte) string {
	s := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(s[:])
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
