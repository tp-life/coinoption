package strategy

import (
	"testing"
	"time"
)

func TestFormatExpTime(t *testing.T) {
	got := FormatExpTime(time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC))
	if got != "2026-11-06 08:00" {
		t.Fatalf("FormatExpTime = %q", got)
	}
}

func TestParseExpiry(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{in: "2026-11-06T08:00:00Z", want: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC), ok: true},
		{in: "2026-11-06", want: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC), ok: true},
		{in: "  2026-11-06T08:00:00Z  ", want: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC), ok: true},
		{in: "not-a-date", ok: false},
		{in: "", ok: false},
	}
	for _, c := range cases {
		got, err := ParseExpiry(c.in)
		if c.ok && err != nil {
			t.Fatalf("ParseExpiry(%q) err = %v", c.in, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("ParseExpiry(%q) 应报错", c.in)
		}
		if c.ok && !got.Equal(c.want) {
			t.Fatalf("ParseExpiry(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestInstExpiryDay(t *testing.T) {
	cases := []struct {
		instID string
		want   string
	}{
		{instID: "BTC-USD-261106-80000-C", want: "2026-11-06"},
		{instID: "BTC-USD_UM-261106-74000-P", want: "2026-11-06"},
		{instID: "BTC-USD-261107-82000-C", want: "2026-11-07"},
		{instID: "bad", want: ""},
	}
	for _, c := range cases {
		if got := InstExpiryDay(c.instID); got != c.want {
			t.Fatalf("InstExpiryDay(%q) = %q, want %q", c.instID, got, c.want)
		}
	}
}

func TestOptTypeNames(t *testing.T) {
	cases := []struct {
		side rune
		opt  string
		dcy  string
	}{
		{side: 'C', opt: "CALL", dcy: "高卖"},
		{side: 'P', opt: "PUT", dcy: "低买"},
		{side: 'c', opt: "CALL", dcy: "高卖"},
		{side: 'p', opt: "PUT", dcy: "低买"},
		{side: 'X', opt: "—", dcy: "—"},
	}
	for _, c := range cases {
		if got := OptTypeName(c.side); got != c.opt {
			t.Fatalf("OptTypeName(%q) = %q, want %q", c.side, got, c.opt)
		}
		if got := DcySideName(c.side); got != c.dcy {
			t.Fatalf("DcySideName(%q) = %q, want %q", c.side, got, c.dcy)
		}
	}
}

func TestSupportedOptionInsts(t *testing.T) {
	ds := []Decision{
		{Row: MatchRow{OptMatched: true, OptInstID: "BTC-USD-261106-80000-C"}},
		{Row: MatchRow{OptMatched: false, OptInstID: ""}},
		{Row: MatchRow{OptMatched: true, OptInstID: "BTC-USD-261107-82000-C"}},
	}
	insts := SupportedOptionInsts(ds)
	if !insts["BTC-USD-261106-80000-C"] || !insts["BTC-USD-261107-82000-C"] {
		t.Fatalf("SupportedOptionInsts = %v", insts)
	}
	if insts["BTC-USD-261106-80000-P"] {
		t.Fatalf("OptMatched=false 的行不应计入 supported: %v", insts)
	}
}
