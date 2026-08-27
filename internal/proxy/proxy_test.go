package proxy

import "testing"

func TestFromEnv(t *testing.T) {
	ups, err := FromEnv([]string{
		"SANDBOX_UPSTREAM_TMDB=https://api.themoviedb.org",
		"SANDBOX_QUERY_TMDB=api_key=abc",
		"SANDBOX_UPSTREAM_PLEX=http://plex:32400",
		"SANDBOX_HEADER_PLEX=X-Plex-Token: xyz",
		"SANDBOX_ALLOW_PLEX=GET /identity\n# libraries\n\nget /library/*\n",
		"UNRELATED=1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 || ups[0].Name != "plex" || ups[1].Name != "tmdb" {
		t.Fatalf("got %+v", ups)
	}
	if ups[0].Header.Get("X-Plex-Token") != "xyz" || ups[1].Query.Get("api_key") != "abc" {
		t.Fatalf("credentials: %+v", ups)
	}
	if len(ups[0].Allow) != 2 || ups[0].Allow[1].String() != "GET /library/*" {
		t.Fatalf("rules: %+v", ups[0].Allow)
	}
	if len(ups[1].Allow) != 0 {
		t.Fatalf("tmdb should have no rules: %+v", ups[1].Allow)
	}
}

func TestRules(t *testing.T) {
	u := Upstream{Allow: []Rule{{"GET", "/identity"}, {"GET", "/library/*"}, {"*", "/any/*"}}}
	allow := [][2]string{
		{"GET", "/identity"}, {"GET", "/library"}, {"GET", "/library/sections"},
		{"GET", "/library/metadata/1/children"}, {"DELETE", "/any/thing"},
	}
	deny := [][2]string{
		{"GET", "/identity/"}, {"GET", "/identityx"}, {"DELETE", "/library/metadata/1"},
		{"GET", "/libraryx"}, {"GET", "/"}, {"POST", "/identity"},
	}
	for _, a := range allow {
		if !u.Allowed(a[0], a[1]) {
			t.Errorf("%s %s should be allowed", a[0], a[1])
		}
	}
	for _, d := range deny {
		if u.Allowed(d[0], d[1]) {
			t.Errorf("%s %s should be denied", d[0], d[1])
		}
	}
	if (Upstream{}).Allowed("GET", "/") {
		t.Error("no rules should deny everything")
	}
}

func TestFromEnvErrors(t *testing.T) {
	for _, env := range [][]string{
		{"SANDBOX_HEADER_PLEX=X-Plex-Token: xyz"},
		{"SANDBOX_UPSTREAM_TMDB=not a url"},
		{"SANDBOX_UPSTREAM_TMDB=https://x", "SANDBOX_HEADER_TMDB=novalue"},
		{"SANDBOX_UPSTREAM_TMDB=https://x", "SANDBOX_QUERY_TMDB=novalue"},
		{"SANDBOX_UPSTREAM_TMDB=https://x", "SANDBOX_ALLOW_TMDB=GET nopath"},
		{"SANDBOX_UPSTREAM_TMDB=https://x", "SANDBOX_ALLOW_TMDB=FETCH /x"},
		{"SANDBOX_ALLOW_TMDB=GET /x"},
	} {
		if _, err := FromEnv(env); err == nil {
			t.Errorf("%v: expected error", env)
		}
	}
}
