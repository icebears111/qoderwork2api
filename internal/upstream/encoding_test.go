package upstream

import (
	"strings"
	"testing"
)

func TestEncodeDecodeRoundtrip(t *testing.T) {
	cases := []string{
		"",
		"a",
		"hi",
		"hello world",
		`{"model":"qmodel_preview","stream":true}`,
		strings.Repeat("x", 1000),
		"中文 UTF-8 内容 ✓",
	}
	for _, s := range cases {
		enc := QoderEncode([]byte(s))
		dec, err := QoderDecode(enc)
		if err != nil {
			t.Errorf("decode %q: %v", s, err)
			continue
		}
		if string(dec) != s {
			t.Errorf("roundtrip %q: got %q", s, dec)
		}
	}
}

func TestEncodeAlphabetAndPad(t *testing.T) {
	enc := QoderEncode([]byte("hi"))
	if strings.Contains(enc, "=") {
		t.Errorf("pad should be $ not =: %q", enc)
	}
	// 所有字符必须在自定义字母表内或是 $
	for _, r := range enc {
		if !strings.ContainsRune(customAlphabet, r) && r != '$' {
			t.Errorf("char %q not in alphabet", r)
		}
	}
}

func TestDecodeInvalidChar(t *testing.T) {
	if _, err := QoderDecode("!!!"); err == nil {
		t.Error("want error for invalid char")
	}
}
