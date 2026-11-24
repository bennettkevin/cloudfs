package names

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Pairs checked by creating the first name on APFS (macOS, case-insensitive
// default) and testing whether the second name then exists. Results recorded
// 2026-11-24 on macOS 26 (Darwin 25.6). TestAPFSLive re-checks them on the
// machine running the tests.
var pairs = []struct {
	a, b     string
	apfsSame bool
}{
	{"notes.md", "NOTES.md", true},
	{"Straße", "STRASSE", true}, // full case folding: ß → ss
	{"Straße", "strasse", true},
	{"ß", "ẞ", true},
	{"ǅ", "ǆ", true}, // titlecase digraph
	{"café", "CAFÉ", true},
	{"café", "café", true}, // NFC vs NFD
	{"ﬁle", "file", true},   // ligature U+FB01 folds to "fi"
	{"Ω", "ω", true},        // OHM SIGN is canonically Ω
	{"ς", "σ", true},        // final sigma
	{"Σ", "σ", true},
	{"Ꭰ", "ꭰ", true},  // Cherokee
	{"İ", "i", false}, // Turkish dotted capital I
	{"ı", "I", false}, // Turkish dotless i
	{"ı", "i", false},
	{"①", "1", false}, // compatibility forms stay distinct (APFS is not NFKC)
	{"Ａ", "A", false}, // fullwidth
	{"ｶ", "カ", false}, // halfwidth katakana
	{"x²", "x2", false},
	{"ǆ", "dž", false},
	{"ﬀ", "ff", true}, // but ligatures that case-fold do match
	{"a", "b", false},
	{"file.txt", "file.txt ", false}, // trailing space is a different name
}

func key(t testing.TB) []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// The blind index must treat names as equal exactly when APFS does.
func TestIndexFormMatchesAPFS(t *testing.T) {
	k := key(t)
	for _, p := range pairs {
		same := bytes.Equal(BlindIndex(k, 1, p.a), BlindIndex(k, 1, p.b))
		if same != p.apfsSame {
			t.Errorf("%q vs %q: index same=%v, APFS same=%v (forms %q, %q)",
				p.a, p.b, same, p.apfsSame, IndexForm(p.a), IndexForm(p.b))
		}
	}
}

// Re-run the APFS check live when the temp dir is on a case-insensitive volume.
func TestAPFSLive(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "Probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "PROBE")); err != nil {
		t.Skip("temp dir is case-sensitive; APFS table not re-checked")
	}
	os.Remove(probe)
	for _, p := range pairs {
		sub := t.TempDir()
		if err := os.WriteFile(filepath.Join(sub, p.a), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(filepath.Join(sub, p.b))
		if got := err == nil; got != p.apfsSame {
			t.Errorf("%q vs %q: filesystem same=%v, table says %v", p.a, p.b, got, p.apfsSame)
		}
	}
}

func TestSameNameDifferentFolder(t *testing.T) {
	k := key(t)
	if bytes.Equal(BlindIndex(k, 1, "README.md"), BlindIndex(k, 2, "README.md")) {
		t.Fatal("same name in different folders must give different indexes")
	}
}

func TestRoundTripAndBinding(t *testing.T) {
	k := key(t)
	for _, name := range []string{"a", "Straße.txt", "café", "报告 2026.pdf", "🙂.png", string(bytes.Repeat([]byte("x"), 255))} {
		ct, err := EncryptName(k, 10, 1, name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecryptName(k, 10, 1, ct)
		if err != nil {
			t.Fatal(err)
		}
		if got != WireForm(name) {
			t.Errorf("round trip %q: got %q", name, got)
		}
		if _, err := DecryptName(k, 11, 1, ct); err == nil {
			t.Error("name moved to another node must not decrypt")
		}
		if _, err := DecryptName(k, 10, 2, ct); err == nil {
			t.Error("node re-parented by the server must not decrypt")
		}
	}
	if _, err := EncryptName(k, 1, 1, string(bytes.Repeat([]byte("x"), 256))); err == nil {
		t.Error("256-byte name must be rejected")
	}
}

func TestPaddingHidesLength(t *testing.T) {
	k := key(t)
	a, _ := EncryptName(k, 1, 1, "a.txt")
	b, _ := EncryptName(k, 1, 1, "longer-name.md")
	if len(a) != len(b) {
		t.Errorf("names under 31 bytes should encrypt to the same size: %d vs %d", len(a), len(b))
	}
	c, _ := EncryptName(k, 1, 1, "a.txt")
	if bytes.Equal(a, c) {
		t.Error("randomized encryption: same name twice must differ")
	}
}

// Size report for the findings doc.
func TestSizeReport(t *testing.T) {
	k := key(t)
	var plain, enc int
	corpus := Corpus(10000)
	for _, n := range corpus {
		ct, err := EncryptName(k, 1, 1, n)
		if err != nil {
			t.Fatal(err)
		}
		plain += len(WireForm(n))
		enc += len(ct)
	}
	runes := 0
	for _, n := range corpus {
		runes += utf8.RuneCountInString(n)
	}
	t.Logf("corpus %d names: avg plaintext %.1f bytes (%.1f chars), avg ciphertext %.1f bytes, + 32-byte index",
		len(corpus), float64(plain)/float64(len(corpus)), float64(runes)/float64(len(corpus)), float64(enc)/float64(len(corpus)))
}

// fold must match Unicode's CaseFolding.txt (C+F) for every code point.
// testdata/CaseFolding-17.0.0.txt is from https://www.unicode.org/Public/17.0.0/ucd/
// and matches the Unicode version of the pinned x/text.
func TestFoldMatchesUnicode(t *testing.T) {
	data, err := os.ReadFile("testdata/CaseFolding-17.0.0.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := map[rune]string{}
	for _, l := range strings.Split(string(data), "\n") {
		if l == "" || l[0] == '#' {
			continue
		}
		p := strings.Split(l, "; ")
		if p[1] != "C" && p[1] != "F" {
			continue
		}
		r, _ := strconv.ParseUint(p[0], 16, 32)
		var s strings.Builder
		for _, h := range strings.Fields(p[2]) {
			c, _ := strconv.ParseUint(h, 16, 32)
			s.WriteRune(rune(c))
		}
		want[rune(r)] = s.String()
	}
	bad := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		exp, ok := want[r]
		if !ok {
			exp = string(r)
		}
		if got := fold(string(r)); got != exp {
			if bad++; bad <= 5 {
				t.Errorf("U+%04X: fold %q, Unicode %q", r, got, exp)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d mismatches", bad)
	}
}
