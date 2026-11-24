package names

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	mrand "math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/miscreant/miscreant.go"
	"golang.org/x/crypto/chacha20poly1305"
)

// Corpus returns n synthetic but realistic names: mostly ASCII, some accented
// Latin, some CJK and emoji, mixed lengths. Deterministic for repeatability.
func Corpus(n int) []string {
	r := mrand.New(mrand.NewPCG(1, 2))
	stems := []string{"IMG_", "report", "Notes", "budget-2026", "Meeting notes", "thesis draft", "invoice", "README",
		"Résumé", "café menu", "Straße", "Überweisung", "naïve plan", "报告", "写真", "회의록", "🙂 holiday", "Ελληνικά"}
	exts := []string{".jpg", ".pdf", ".docx", ".md", ".txt", ".go", ".png", ".xlsx", ""}
	out := make([]string, n)
	for i := range out {
		s := stems[r.IntN(len(stems))]
		var b strings.Builder
		b.WriteString(s)
		switch r.IntN(4) {
		case 0:
			fmt.Fprintf(&b, "%04d", r.IntN(10000))
		case 1:
			fmt.Fprintf(&b, " (%d)", r.IntN(10))
		case 2:
			b.WriteString(" final v" + fmt.Sprint(r.IntN(5)) + " - " + strings.Repeat("long ", r.IntN(8)))
		}
		b.WriteString(exts[r.IntN(len(exts))])
		out[i] = b.String()
	}
	return out
}

var corpus = Corpus(10000)

func BenchmarkIndexForm(b *testing.B) {
	for i := 0; b.Loop(); i++ {
		IndexForm(corpus[i%len(corpus)])
	}
}

func BenchmarkBlindIndex(b *testing.B) {
	k := key(b)
	for i := 0; b.Loop(); i++ {
		BlindIndex(k, 42, corpus[i%len(corpus)])
	}
}

func BenchmarkEncryptName(b *testing.B) {
	k := key(b)
	for i := 0; b.Loop(); i++ {
		if _, err := EncryptName(k, uint64(i), 42, corpus[i%len(corpus)]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecryptName(b *testing.B) {
	k := key(b)
	cts := make([][]byte, len(corpus))
	for i, n := range corpus {
		cts[i], _ = EncryptName(k, uint64(i), 42, n)
	}
	for i := 0; b.Loop(); i++ {
		j := i % len(corpus)
		if _, err := DecryptName(k, uint64(j), 42, cts[j]); err != nil {
			b.Fatal(err)
		}
	}
}

// Encrypt + index together: the per-name cost a client pays when committing.
func BenchmarkEncryptAndIndex(b *testing.B) {
	k1, k2 := key(b), key(b)
	for i := 0; b.Loop(); i++ {
		n := corpus[i%len(corpus)]
		EncryptName(k1, uint64(i), 42, n)
		BlindIndex(k2, 42, n)
	}
}

// Deterministic alternative (what Syncthing and Cryptomator use), for comparison.
func BenchmarkAESSIVName(b *testing.B) {
	k := make([]byte, 64)
	rand.Read(k)
	c, err := miscreant.NewAESCMACSIV(k)
	if err != nil {
		b.Fatal(err)
	}
	ad := nameAD(1, 42)
	for i := 0; b.Loop(); i++ {
		if _, err := c.Seal(nil, []byte(WireForm(corpus[i%len(corpus)])), ad); err != nil {
			b.Fatal(err)
		}
	}
}

// Baselines: what a client already pays per chunk (ADR-0009/0010).
func BenchmarkChunkEncrypt1MiB(b *testing.B) {
	aead, _ := chacha20poly1305.NewX(key(b))
	pt := make([]byte, 1<<20)
	nonce := make([]byte, aead.NonceSize())
	out := make([]byte, 0, len(pt)+aead.Overhead())
	b.SetBytes(int64(len(pt)))
	for b.Loop() {
		aead.Seal(out[:0], nonce, pt, nil)
	}
}

func BenchmarkChunkID1MiB(b *testing.B) {
	m := hmac.New(sha256.New, key(b))
	pt := make([]byte, 1<<20)
	b.SetBytes(int64(len(pt)))
	for b.Loop() {
		m.Reset()
		m.Write(pt)
		m.Sum(nil)
	}
}

// Wall-clock for an initial sync's worth of names, single goroutine.
func TestInitialSyncNames(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	k1, k2 := key(t), key(t)
	for _, n := range []int{10_000, 100_000} {
		names := Corpus(n)
		start := time.Now()
		for i, name := range names {
			if _, err := EncryptName(k1, uint64(i), 42, name); err != nil {
				t.Fatal(err)
			}
			BlindIndex(k2, 42, name)
		}
		enc := time.Since(start)
		start = time.Now()
		for _, name := range names {
			_ = WireForm(name)
		}
		t.Logf("%d names: encrypt+index %v total; NFC alone (needed anyway) %v", n, enc.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	}
}
