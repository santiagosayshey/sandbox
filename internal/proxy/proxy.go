// Package proxy forwards requests to configured upstreams, injecting the
// credential each one needs so clients never hold it.
package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
)

const (
	envUpstream = "SANDBOX_UPSTREAM_"
	envHeader   = "SANDBOX_HEADER_"
	envQuery    = "SANDBOX_QUERY_"
	envAllow    = "SANDBOX_ALLOW_"
)

// Upstream is one proxied service.
type Upstream struct {
	Name   string // route prefix, lower case
	URL    *url.URL
	Header http.Header // injected on every request
	Query  url.Values  // injected on every request
	Allow  []Rule      // requests that may be forwarded; empty denies all
}

// Rule is one allowed request shape: a method ("*" for any) and a path.
// A path ending in "/*" matches itself and anything beneath it; any other
// path must match exactly.
type Rule struct {
	Method string
	Path   string
}

func (r Rule) String() string { return r.Method + " " + r.Path }

// Matches reports whether the rule allows the request.
func (r Rule) Matches(method, p string) bool {
	if r.Method != "*" && r.Method != method {
		return false
	}
	if strings.HasSuffix(r.Path, "/*") {
		prefix := strings.TrimSuffix(r.Path, "/*")
		return p == prefix || strings.HasPrefix(p, prefix+"/")
	}
	return p == r.Path
}

// Allowed reports whether any rule allows the request.
func (u Upstream) Allowed(method, p string) bool {
	for _, r := range u.Allow {
		if r.Matches(method, p) {
			return true
		}
	}
	return false
}

// parseRules reads "METHOD /path" lines; blank lines and # comments are
// skipped.
func parseRules(v string) ([]Rule, error) {
	var rules []Rule
	for _, line := range strings.Split(v, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		method, p, ok := strings.Cut(line, " ")
		p = strings.TrimSpace(p)
		if !ok || p == "" || !strings.HasPrefix(p, "/") {
			return nil, fmt.Errorf("rule %q: expected \"METHOD /path\"", line)
		}
		method = strings.ToUpper(method)
		if method != "*" && !isMethod(method) {
			return nil, fmt.Errorf("rule %q: unknown method", line)
		}
		rules = append(rules, Rule{Method: method, Path: p})
	}
	return rules, nil
}

func isMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	}
	return false
}

// FromEnv builds the upstream table from SANDBOX_UPSTREAM_<NAME>,
// SANDBOX_HEADER_<NAME> ("Name: value"), SANDBOX_QUERY_<NAME> ("key=value")
// and SANDBOX_ALLOW_<NAME> ("METHOD /path" lines) variables. env is in
// os.Environ form.
func FromEnv(env []string) ([]Upstream, error) {
	byName := map[string]*Upstream{}
	get := func(name string) *Upstream {
		u, ok := byName[name]
		if !ok {
			u = &Upstream{Name: name, Header: http.Header{}, Query: url.Values{}}
			byName[name] = u
		}
		return u
	}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(k, envUpstream):
			name := strings.ToLower(strings.TrimPrefix(k, envUpstream))
			target, err := url.Parse(v)
			if err != nil || target.Scheme == "" || target.Host == "" {
				return nil, fmt.Errorf("%s: not an absolute URL: %q", k, v)
			}
			get(name).URL = target
		case strings.HasPrefix(k, envHeader):
			name := strings.ToLower(strings.TrimPrefix(k, envHeader))
			hk, hv, ok := strings.Cut(v, ":")
			if !ok || strings.TrimSpace(hk) == "" {
				return nil, fmt.Errorf("%s: expected \"Name: value\"", k)
			}
			get(name).Header.Add(strings.TrimSpace(hk), strings.TrimSpace(hv))
		case strings.HasPrefix(k, envQuery):
			name := strings.ToLower(strings.TrimPrefix(k, envQuery))
			qk, qv, ok := strings.Cut(v, "=")
			if !ok || qk == "" {
				return nil, fmt.Errorf("%s: expected \"key=value\"", k)
			}
			get(name).Query.Add(qk, qv)
		case strings.HasPrefix(k, envAllow):
			name := strings.ToLower(strings.TrimPrefix(k, envAllow))
			rules, err := parseRules(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			get(name).Allow = append(get(name).Allow, rules...)
		}
	}
	var out []Upstream
	for name, u := range byName {
		if u.URL == nil {
			return nil, fmt.Errorf("upstream %q has a header, query or allow list but no %s%s", name, envUpstream, strings.ToUpper(name))
		}
		if name == "" || strings.ContainsAny(name, "/ ") {
			return nil, fmt.Errorf("upstream name %q is not usable as a route", name)
		}
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Handler returns a handler that expects the "/<name>" prefix already
// stripped, refuses anything the allow list does not cover, and forwards
// the rest to the upstream.
func (u Upstream) Handler() http.Handler {
	rp := u.reverseProxy()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !u.Allowed(r.Method, r.URL.Path) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, "%s %s is not allowed on upstream %s.\n", r.Method, r.URL.Path, u.Name)
			if len(u.Allow) == 0 {
				fmt.Fprintln(w, "Nothing is allowed on this upstream.")
				return
			}
			fmt.Fprintln(w, "Allowed:")
			for _, rule := range u.Allow {
				fmt.Fprintf(w, "  %s\n", rule)
			}
			return
		}
		rp.ServeHTTP(w, r)
	})
}

func (u Upstream) reverseProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		// Say why in the body, not only in the log, so a client sees
		// "no such host" rather than an empty 502.
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, fmt.Sprintf("upstream %s: %v", u.Name, err), http.StatusBadGateway)
		},
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u.URL)
			r.Out.Host = u.URL.Host
			// Never forward a client-supplied credential; the upstream's
			// comes from the configuration below.
			r.Out.Header.Del("Authorization")
			for k, vs := range u.Header {
				r.Out.Header.Del(k)
				for _, v := range vs {
					r.Out.Header.Add(k, v)
				}
			}
			if len(u.Query) > 0 {
				q := r.Out.URL.Query()
				for k, vs := range u.Query {
					q.Del(k)
					for _, v := range vs {
						q.Add(k, v)
					}
				}
				r.Out.URL.RawQuery = q.Encode()
			}
		},
	}
}
