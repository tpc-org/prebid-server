package config

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type testStruct struct {
	Myint          int    `mapstructure:"this_int"`
	Mystring       string `mapstructure:"mystring"`
	Flag           bool
	Sub            innerStruct `mapstructure:"sub"`
	Caps           map[string]string
	NestedStruct   *nestedStruct
	NillableStruct *nestedStruct
}

type innerStruct struct {
	int1     int    `mapstructure:"int1"`
	password string `mapstructure:"password"`
}

type nestedStruct struct {
	var1 string
}

var expected string = `this_int: 5
mystring: foobar
((Flag)): false
sub.int1: 3
sub.password: <REDACTED>
((Caps))[Alabama]: Montgomery
((NestedStruct)).((var1)): abc
`

func TestBasic(t *testing.T) {
	var buf bytes.Buffer

	mylogger := func(msg string, args ...interface{}) {
		buf.WriteString(fmt.Sprintf(fmt.Sprintln(msg), args...))
	}

	testCfg := testStruct{
		Myint:    5,
		Mystring: "foobar",
		Sub: innerStruct{
			int1:     3,
			password: "secret",
		},
		// Can't do more than one entry as order is not guaranteed.
		Caps: map[string]string{
			"Alabama": "Montgomery",
		},
		NestedStruct:   &nestedStruct{var1: "abc"},
		NillableStruct: nil,
	}

	logStructWithLogger(reflect.ValueOf(testCfg), "", mylogger)

	result := buf.String()

	if expected != result {
		t.Errorf("Did not log properly.\ndesired:%s\nfound:%s\nsource: %v", expected, result, testCfg)
	}
}

// TestCredentialsRedacted (TPC fork): the startup config dump must never print
// partner credentials — adapters' extra_info carries API keys in this fork.
func TestCredentialsRedacted(t *testing.T) {
	var buf bytes.Buffer
	mylogger := func(msg string, args ...interface{}) {
		buf.WriteString(fmt.Sprintf(fmt.Sprintln(msg), args...))
	}

	bidders := BidderInfos{"thrad": BidderInfo{
		Endpoint:         "https://ssp.thrad.ai/api/v2/bid-request",
		ExtraAdapterInfo: `{"productionKey":"sk_live_do_not_log"}`,
		AppSecret:        "app-secret-do-not-log",
	}}
	logMapWithLogger(reflect.ValueOf(bidders), "adapters", mylogger)
	logStructWithLogger(reflect.ValueOf(struct {
		ClientKey     string `mapstructure:"client_key"`
		OverrideToken string `mapstructure:"override_token"`
		Keyword       string `mapstructure:"keyword"`
	}{"client-key-do-not-log", "token-do-not-log", "visible"}), "", mylogger)

	out := buf.String()
	if strings.Contains(out, "do-not-log") || strings.Contains(out, "do_not_log") {
		t.Fatalf("credential logged:\n%s", out)
	}
	for _, want := range []string{
		"adapters[thrad].extra_info: <REDACTED>",
		"adapters[thrad].app_secret: <REDACTED>",
		"client_key: <REDACTED>",
		"override_token: <REDACTED>",
		"adapters[thrad].endpoint: https://ssp.thrad.ai/api/v2/bid-request",
		"keyword: visible",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
