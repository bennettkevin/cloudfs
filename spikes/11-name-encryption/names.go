// Package names is spike 11's prototype of encrypted file names plus a blind index.
//
// It is spike code: it exists to measure cost and check Unicode behaviour,
// not to be imported by CloudFS. See docs/research/11-name-encryption.md.
package names

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// padBlock is the size names are padded up to before encryption, so the
// ciphertext reveals the name length only to within 32 bytes.
const padBlock = 32

// MaxNameBytes is the longest name accepted (common filesystem limit).
const MaxNameBytes = 255

var folder = cases.Fold()

// IndexForm is the form of a name that the blind index hashes. Two names with
// the same IndexForm are "the same name" for uniqueness.
//
// It follows Unicode canonical caseless matching (Unicode §3.13, D145), which
// compares NFD(fold(NFD(x))). We normalize the folded string to NFC instead of
// NFD; NFC and NFD are both canonical forms, so the same names come out equal.
func IndexForm(name string) string {
	return norm.NFC.String(fold(norm.NFD.String(name)))
}

// fold is full default case folding (Unicode CaseFolding.txt, statuses C+F).
//
// x/text's cases.Fold is correct except for Cherokee: Unicode folds Cherokee
// to the *capital* letters (U+13A0–U+13F5), but x/text folds the capitals to
// the small letters, so "Ꭰ" and "ꭰ" fold differently (golang/go#46101, open
// since 2021). We map Cherokee small letters back to capitals afterwards.
// TestFoldMatchesUnicode checks every code point against CaseFolding.txt.
func fold(s string) string {
	f := folder.String(s)
	if !strings.ContainsFunc(f, isCherokeeSmall) {
		return f
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0xAB70 && r <= 0xABBF: // CHEROKEE SMALL LETTER A … MV
			return r - 0xAB70 + 0x13A0
		case r >= 0x13F8 && r <= 0x13FD: // CHEROKEE SMALL LETTER YE … MV
			return r - 0x13F8 + 0x13F0
		}
		return r
	}, f)
}

func isCherokeeSmall(r rune) bool {
	return (r >= 0xAB70 && r <= 0xABBF) || (r >= 0x13F8 && r <= 0x13FD)
}

// WireForm is the name as sent to the server inside the ciphertext: NFC,
// with the user's casing preserved (ADR-0014).
func WireForm(name string) string { return norm.NFC.String(name) }

// BlindIndex is HMAC-SHA256(indexKey, parentID || IndexForm(name)).
// Including the parent means equal names in different folders produce
// different indexes, so the server learns only what it needs: whether two
// names in the same folder collide.
func BlindIndex(indexKey []byte, parentID uint64, name string) []byte {
	m := hmac.New(sha256.New, indexKey)
	var p [8]byte
	binary.BigEndian.PutUint64(p[:], parentID)
	m.Write(p[:])
	m.Write([]byte(IndexForm(name)))
	return m.Sum(nil)
}

// nameAD binds a name ciphertext to its node and parent, so the server can't
// move a name to another node or silently re-parent a node.
func nameAD(nodeID, parentID uint64) []byte {
	ad := make([]byte, 16)
	binary.BigEndian.PutUint64(ad[:8], nodeID)
	binary.BigEndian.PutUint64(ad[8:], parentID)
	return ad
}

// EncryptName returns nonce || XChaCha20-Poly1305(pad(WireForm(name))).
func EncryptName(nameKey []byte, nodeID, parentID uint64, name string) ([]byte, error) {
	w := WireForm(name)
	if len(w) == 0 || len(w) > MaxNameBytes {
		return nil, errors.New("name length out of range")
	}
	aead, err := chacha20poly1305.NewX(nameKey)
	if err != nil {
		return nil, err
	}
	// Padding: 1 length byte, the name, zero bytes up to a multiple of padBlock.
	n := 1 + len(w)
	n = (n + padBlock - 1) / padBlock * padBlock
	pt := make([]byte, n)
	pt[0] = byte(len(w))
	copy(pt[1:], w)

	out := make([]byte, aead.NonceSize(), aead.NonceSize()+n+aead.Overhead())
	if _, err := rand.Read(out); err != nil {
		return nil, err
	}
	return aead.Seal(out, out, pt, nameAD(nodeID, parentID)), nil
}

// DecryptName reverses EncryptName. It fails if the ciphertext was moved to a
// different node or parent.
func DecryptName(nameKey []byte, nodeID, parentID uint64, ct []byte) (string, error) {
	aead, err := chacha20poly1305.NewX(nameKey)
	if err != nil {
		return "", err
	}
	if len(ct) < aead.NonceSize()+aead.Overhead() {
		return "", errors.New("ciphertext too short")
	}
	pt, err := aead.Open(nil, ct[:aead.NonceSize()], ct[aead.NonceSize():], nameAD(nodeID, parentID))
	if err != nil {
		return "", err
	}
	l := int(pt[0])
	if l == 0 || 1+l > len(pt) {
		return "", errors.New("bad padding")
	}
	return string(pt[1 : 1+l]), nil
}
