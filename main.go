package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	authchallenge "github.com/docker/distribution/registry/client/auth/challenge"
	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/imjasonh/tlogistry/internal/rekor"
	"github.com/kelseyhightower/envconfig"
)

func main() {
	var env struct {
		Port int64 `envconfig:"PORT" default:"8080"`
	}
	if err := envconfig.Process("", &env); err != nil {
		log.Fatalf("envconfig: %v", err)
	}

	http.HandleFunc("/", handleHome)
	http.HandleFunc("/style.css", handleStyle)
	http.HandleFunc("/v2/", handler)

	log.Printf("Listening on port %d", env.Port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", env.Port), nil))
}

//go:embed README.md
var readmeMD []byte
var readmeHTML []byte
var homeOnce sync.Once

//go:embed style.css
var style []byte

func handleHome(w http.ResponseWriter, _ *http.Request) {
	homeOnce.Do(func() {
		readmeHTML = markdown.ToHTML(readmeMD,
			parser.NewWithExtensions(parser.CommonExtensions),
			html.NewRenderer(html.RendererOptions{
				CSS:   "style.css",
				Title: "tlog.kontain.me",
				Flags: html.CommonFlags | html.CompletePage | html.HrefTargetBlank,
			}))
	})
	if _, err := w.Write(readmeHTML); err != nil {
		log.Printf("!!! ERROR WRITING HTML: %v", err)
	}
}

func handleStyle(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css")
	if _, err := w.Write(style); err != nil {
		log.Printf("!!! ERROR WRITING STYLE: %v", err)
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	log.Println("handler:", r.Method, r.URL)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		serveError(w, regError{status: http.StatusMethodNotAllowed, Code: "DENIED", Message: "tlogistry is read-only"})
		return
	}

	switch r.URL.Path {
	case "/v2/", "/v2":
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	default:
		proxy(w, r)
	}
}

func proxy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	parts := strings.Split(r.URL.Path, "/")

	// /v2/ubuntu/manifests/latest -> ubuntu
	// /v2/example.biz/foo/bar/manifests/latest -> example.biz/foo/bar
	repostr := strings.Join(parts[2:len(parts)-2], "/")
	repo, err := name.NewRepository(repostr)
	if err != nil {
		serveError(w, regError{status: http.StatusBadRequest, Code: "NAME_INVALID", Message: fmt.Sprintf("parsing repository name: %v", err)})
		return
	}

	url := fmt.Sprintf("https://%s/v2/%s/%s", repo.RegistryStr(), repo.RepositoryStr(), strings.Join(parts[len(parts)-2:], "/"))
	log.Println("-->", r.Method, r.URL)
	req, _ := http.NewRequest(r.Method, url, nil)
	for k, v := range r.Header {
		for _, vv := range v {
			req.Header.Add(k, vv)
			if k == "Authorization" {
				vv = "REDACTED"
			}
			log.Printf("--> %s: %s", k, vv)
		}
	}

	isManifestTagRequest := parts[len(parts)-2] == "manifests" &&
		!strings.HasPrefix(parts[len(parts)-1], "sha256:")

	// If this is a request for manifest by tag, check Rekor to see if we have a digest for it.
	var tag name.Tag
	var wantDigest string
	var info *rekor.Info
	if isManifestTagRequest {
		tagstr := parts[len(parts)-1]
		var err error
		tag, err = name.NewTag(fmt.Sprintf("%s:%s", repo.String(), tagstr))
		if err != nil {
			serveError(w, regError{status: http.StatusBadRequest, Code: "NAME_INVALID", Message: fmt.Sprintf("parsing tag: %v", err)})
			return
		}
		wantDigest, info, err = rekor.Get(ctx, tag)
		if err != nil {
			serveError(w, newRegError(fmt.Errorf("looking up digest for tag %q: %v", tag, err)))
			return
		}
		log.Println("=== REKOR: found digest for tag", tag, wantDigest)
	}

	// If the request is coming in without auth, get some auth.
	//
	// It's unlikely the request comes in with auth already attached, since
	// that would have required /v2 to point to /token and for /token to
	// have generated some creds.
	if req.Header.Get("Authorization") == "" {
		log.Println("  Getting token...")
		t, err := getToken(repo)
		if err != nil {
			serveError(w, newRegError(fmt.Errorf("getting token: %v", err)))
			return
		}
		req.Header.Set("Authorization", "Bearer "+t)
	}

	resp, err := http.DefaultTransport.RoundTrip(req) // Transport doesn't follow redirects.
	if err != nil {
		serveError(w, newRegError(fmt.Errorf("fetching %q: %v", url, err)))
		return
	}
	defer resp.Body.Close()

	gotDigest := resp.Header.Get("Docker-Content-Digest")
	if wantDigest != "" && gotDigest != wantDigest {
		serveError(w, digestMismatch(tag.String(), gotDigest, wantDigest))
		return
	}

	log.Println("<--", resp.StatusCode)
	for k, v := range resp.Header {
		for _, vv := range v {
			log.Printf("<-- %s: %s", k, vv)
			w.Header().Add(k, vv)
		}
	}

	if isManifestTagRequest && // If this is a request for manifest by tag,
		gotDigest != "" && // and we have the digest now,
		wantDigest == "" { // and we didn't have one before --> record it in Rekor.
		log.Println("=== REKOR: writing digest for tag", tag, gotDigest)
		if info, err = rekor.Put(ctx, tag, gotDigest); err != nil {
			log.Println("!!! ERROR WRITING TO REKOR:", err)
		}
		// This request made us write an entry for the first time.
		w.Header().Set("TLog-First-Seen", "true")
	}

	if info != nil {
		w.Header().Set("TLog-UUID", info.UUID)
		w.Header().Set("TLog-LogIndex", fmt.Sprintf("%d", info.LogIndex))
		w.Header().Set("TLog-IntegratedTime", info.IntegratedTime.Format(time.RFC3339))
	}
	w.WriteHeader(resp.StatusCode)
	if parts[len(parts)-2] != "blobs" { // Never proxy blobs.
		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Println("!!! ERROR COPYING RESPONSE BODY:", err)
		}
	}
}

func getToken(repo name.Repository) (string, error) {
	// Ping /v2/, determine the registry's auth scheme.
	url := fmt.Sprintf("https://%s/v2/", repo.RegistryStr())
	log.Println("  --> GET", url)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	log.Println("  <--", resp.StatusCode)
	for k, v := range resp.Header {
		for _, vv := range v {
			log.Printf("  <-- %s: %s", k, vv)
		}
	}
	if resp.StatusCode == http.StatusOK {
		return "", nil // Registry doesn't require auth.
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return "", fmt.Errorf("unexpected status code (%s): %d", url, resp.StatusCode)
	}
	chs := authchallenge.ResponseChallenges(resp)
	if len(chs) == 0 {
		return "", nil // Registry doesn't require auth.
	}
	if strings.ToLower(chs[0].Scheme) != "bearer" {
		return "", fmt.Errorf("unsupported auth scheme: %s", chs[0].Scheme)
	}

	// Ping token endpoint, get a token.
	service := chs[0].Parameters["service"]
	realm := chs[0].Parameters["realm"]
	url = fmt.Sprintf("%s?scope=repository:%s:pull&service=%s", realm, repo.RepositoryStr(), service)
	log.Println("  --> GET", url)
	resp, err = http.Get(url)
	if err != nil {
		return "", err
	}
	log.Println("  <--", resp.StatusCode)
	for k, v := range resp.Header {
		for _, vv := range v {
			log.Printf("  <-- %s: %s", k, vv)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code (%s): %d", url, resp.StatusCode)
	}
	defer resp.Body.Close()
	var tokenResp struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}
	return tokenResp.Token, nil
}

func serveError(w http.ResponseWriter, re regError) {
	http.Error(w, "", re.status)
	if err := json.NewEncoder(w).Encode(&resp{
		Errors: []regError{re},
	}); err != nil {
		log.Printf("!!! ERROR WRITING ERROR BODY (%+v): %v", re, err)
	}
}

type resp struct {
	Errors []regError `json:"errors"`
}

type regError struct {
	status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func digestMismatch(tag, got, want string) regError {
	return regError{
		status:  http.StatusBadRequest,
		Code:    "TAG_INVALID",
		Message: fmt.Sprintf("tag %q mismatch; got %q, want %q", tag, got, want),
	}
}

func newRegError(err error) regError {
	return regError{
		status:  http.StatusInternalServerError,
		Code:    "INTERNAL_ERROR",
		Message: err.Error(),
	}
}
