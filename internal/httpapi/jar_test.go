package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
)

func cookieJar() (http.CookieJar, error) { return cookiejar.New(nil) }

func jsonDecode(resp *http.Response, v any) error { return json.NewDecoder(resp.Body).Decode(v) }
