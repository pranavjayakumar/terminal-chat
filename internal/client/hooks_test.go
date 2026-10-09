package client

import (
	"strings"
	"testing"
)

const goodPin = "sha256//2ImGzJcJbP/yiBQfiM3qKb091xniRQuJZZBbh5NGL1M="

// TestHookCurl: the client builds the curl command itself and refuses
// anything from the server that could break out of its quoting (L3, L4).
func TestHookCurl(t *testing.T) {
	url := "https://chat.example.com:7443/hooks/zxjwzfyycpkafphffrnckqmw5u"
	curl, err := hookCurl(url, goodPin)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(curl, "\n"), "\n")
	if len(lines) != 2 || lines[0] != "# export TC_HOOK_URL='"+url+"'" ||
		!strings.HasSuffix(lines[1], ` "$TC_HOOK_URL"`) || strings.Contains(lines[1], url) ||
		!strings.Contains(lines[1], "--pinnedpubkey '"+goodPin+"'") {
		t.Fatalf("curl:\n%s", curl)
	}
	for _, u := range []string{
		"https://[::1]:7443/hooks/abc", "https://10.0.0.1:7443/hooks/abc",
	} {
		if _, err := hookCurl(u, goodPin); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
	for _, u := range []string{
		"https://h'$(touch /tmp/x)':7443/hooks/abc",
		"https://h:7443/hooks/abc'; rm -rf ~; '",
		"https://h:7443/hooks/abc\nrm -rf ~",
		"http://h:7443/hooks/abc",
		"https://h:7443/other/abc",
		"https://h:7443/hooks/ABC",
		"https://h:7443/hooks/",
		"https://h\":7443/hooks/abc",
		"https://h 1:7443/hooks/abc",
	} {
		if _, err := hookCurl(u, goodPin); err == nil {
			t.Errorf("url %q accepted", u)
		}
	}
	for _, p := range []string{
		"", "sha256//", "sha256//x=", "sha256//" + strings.Repeat("A", 44),
		"sha256//2ImGzJcJbP/yiBQfiM3qKb091xniRQuJZZBbh5NGL1M=' ; id #",
		"2ImGzJcJbP/yiBQfiM3qKb091xniRQuJZZBbh5NGL1M=",
		"sha256//2ImGzJcJbP_yiBQfiM3qKb091xniRQuJZZBbh5NGL1M=",
	} {
		if _, err := hookCurl(url, p); err == nil {
			t.Errorf("pin %q accepted", p)
		}
	}
}
