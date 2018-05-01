/* Copyright © Playground Global, LLC. All rights reserved. */

package main

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"playground/config"
	"playground/httputil"
	"playground/httputil/static"
	"playground/log"
	"playground/session"
)

/*
 * Configuration type & helpers
 */

type serverConfig struct {
	Debug         bool
	Port          int
	HTTPPort      int
	BindAddress   string
	LogFile       string
	APIServerURL  string
	StaticContent string
	AdminUsers    []string
	HTTPSCertFile string
	HTTPSKeyFile  string
	Session       *session.ConfigType
	APIClient     *httputil.ConfigType
}

var cfg = &serverConfig{
	false,
	9000,
	0,
	"",
	"./bifrost.log",
	"https://localhost:9090/",
	"./static",
	[]string{},
	"",
	"",
	&session.Config,
	&httputil.Config,
}

func initConfig(cfg *serverConfig) {
	config.Load(cfg)

	if cfg.LogFile != "" {
		log.SetLogFile(cfg.LogFile)
	}
	if config.Debug || cfg.Debug {
		log.SetLogLevel(log.LEVEL_DEBUG)
	}
}

func main() {
	initConfig(cfg)
	session.Ready()

	// static content & OAuth2/session handlers
	handler := static.Content{Path: cfg.StaticContent, Prefix: "/static/"}
	if !config.Debug && !cfg.Debug {
		handler.Preload("index.html", "favicon.ico")
	}
	http.HandleFunc("/", handler.RootHandler)
	http.HandleFunc("/favicon.ico", handler.FaviconHandler)
	http.HandleFunc("/static/", handler.Handler)
	http.HandleFunc(session.Config.OAuth.RedirectPath, static.OAuthHandler)

	// API endpoints
	httputil.HandleFunc("/api/init", []string{"GET"}, initHandler)
	httputil.HandleFunc("/api/config", []string{"GET", "PUT"}, configHandler)
	httputil.HandleFunc("/api/whitelist", []string{"GET"}, whitelistHandler)
	httputil.HandleFunc("/api/whitelist/", []string{"PUT", "DELETE"}, whitelistHandler)
	httputil.HandleFunc("/api/users", []string{"GET"}, usersHandler)
	httputil.HandleFunc("/api/users/", []string{"GET", "PUT", "DELETE"}, usersHandler)
	httputil.HandleFunc("/api/certs", []string{"GET", "POST"}, certsHandler)
	httputil.HandleFunc("/api/certs/", []string{"DELETE"}, certsHandler)
	httputil.HandleFunc("/api/totp", []string{"GET", "POST"}, totpHandler)
	httputil.HandleFunc("/api/events", []string{"GET"}, eventsHandler)

	tlsConfig := &tls.Config{
		PreferServerCipherSuites: true,
		CurvePreferences: []tls.CurveID{
			tls.CurveP256,
			tls.X25519,
		},
		MinVersion: tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			// tls.TLS_RSA_WITH_AES_256_GCM_SHA384, // no PFS
			// tls.TLS_RSA_WITH_AES_128_GCM_SHA256, // no PFS
		},
	}

	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.BindAddress, cfg.Port),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
		TLSConfig:    tlsConfig,
		Handler:      http.DefaultServeMux,
	}

	if cfg.HTTPSCertFile != "" { // HTTPS mode -- not behind reverse proxy
		// if a bare-HTTP port was also specified, start up a server on that that redirects to HTTPS with HSTS
		if cfg.HTTPPort > 0 {
			go func() {
				log.Warn("main (http)", "fallback HTTP server shutting down", (&http.Server{
					ReadTimeout:  5 * time.Second,
					WriteTimeout: 5 * time.Second,
					IdleTimeout:  120 * time.Second,
					Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						w.Header().Set("Connection", "close")
						if !cfg.Debug {
							w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
						}
						port := ""
						if cfg.Port != 443 {
							port = fmt.Sprintf(":%d", cfg.HTTPPort)
						}
						url := fmt.Sprintf("https://%s%s/%s", req.Host, port, req.URL.String())
						log.Debug("main (http)", "redirect to https", url)
						http.Redirect(w, req, url, http.StatusMovedPermanently)
					}),
				}).ListenAndServe())
			}()
		}

		// start the main HTTPS server
		log.Error("main (https)", "shutting down", server.ListenAndServeTLS(cfg.HTTPSCertFile, cfg.HTTPSKeyFile))
	} else { // HTTP mode -- behind reverse proxy (hopefully)
		log.Error("main (http)", "shutting down", server.ListenAndServe())
	}
}

/*
 * Package-local utilities
 */

func extractSegment(path string, n int) string {
	chunks := strings.Split(path, "/")
	if len(chunks) > n {
		return chunks[n]
	}
	return ""
}

// create some frequently-used error responses for readability later
var (
	authError       = &apiError{"You must be logged in to use this application.", "Please reload the page.", false}
	eventsError     = &apiError{"You must be an administrator to view events.", "", false}
	clientJSONError = &apiError{"There was an error in data your client sent.", "Please reload the page.", false}
	clientURLError  = &apiError{"There was an error in data your client sent.", "Please reload the page.", false}
	settingsError   = &apiError{"You must be an administrator to access settings.", "", false}
	usersError      = &apiError{"You must be an administrator to manage users.", "", false}
)

/* All handlers that return JSON use this general structure:
 *
 * {
 *   "Error": { "Message": "", "Extra": "", "Recoverable": false },
 *   "Artifact": { ... }
 * }
 *
 * Error is null, empty, or not present on a successful (200-series) response. The Artifact
 * sub-object contains actual data. The response objects documented in the handlers below are
 * actually nested in the response as Artifact.
 */
type apiError struct {
	Message     string
	Extra       string
	Recoverable bool
}
type apiResponse struct {
	Error    *apiError   `json:",omitEmpty"`
	Artifact interface{} `json:",omitEmpty"`
}

type settings struct {
	ServiceName                     string
	ClientLimit, IssuedCertDuration int
	WhitelistedDomains              []string
	WhitelistedUsers                []string `json:",omitEmpty"`
}

func loadSession(req *http.Request) (ssn *session.Session, s *settings, isAllowed bool, isAdmin bool) {
	s = &settings{}
	if ssn = session.GetSession(req); !ssn.IsLoggedIn() {
		return
	}

	status, err := httputil.CallAPI(httputil.URLJoin(cfg.APIServerURL, "settings"), "GET", struct{}{}, s)
	if err != nil {
		panic(err)
	}
	if status >= 300 {
		panic(fmt.Sprintf("non-200 status code %d from API server", status))
	}

	for _, email := range cfg.AdminUsers {
		if email == ssn.Email {
			isAdmin = true
			isAllowed = true
			break
		}
	}

	if !isAdmin {
		for _, domain := range s.WhitelistedDomains {
			if strings.HasSuffix(ssn.Email, fmt.Sprintf("@%s", domain)) {
				isAllowed = true
				break
			}
		}
		if !isAllowed {
			for _, email := range s.WhitelistedUsers {
				if ssn.Email == email {
					isAllowed = true
					break
				}
			}
		}
	}

	return
}

/*
 * API endpoint handlers
 */

func initHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /init -- fetch initial client state
	//   I: none
	//   O: {IsAdmin: false, ServiceTitle: "", ServiceName: "", DefaultPath: "", MaxClients: 42}
	//   200: success
	// non-GET: 405 (method not allowed)

	ssn, s, isAllowed, isAdmin := loadSession(req)
	if !ssn.IsLoggedIn() {
		httputil.SendJSON(writer, http.StatusForbidden, &apiResponse{Error: authError})
		return
	}

	res := &struct {
		IsAdmin                  bool
		ServiceName, DefaultPath string
		MaxClients               int
	}{
		false, "Bifröst VPN", "/sorry", 2,
	}

	res.ServiceName = s.ServiceName
	res.MaxClients = s.ClientLimit
	if isAllowed {
		res.DefaultPath = "/devices"
	}
	if isAdmin {
		res.IsAdmin = true
		res.DefaultPath = "/users"
	}

	// note: other handlers check isAllowed and 403 if false, but we can't since we are init and need
	// to tell the client it isn't allowed so it can show the correct UI

	httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, res})
}

func configHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /api/config -- fetch current app configuration settings
	//   I: none
	//   O: {ClientLimit: 2, ServiceName: "", IssuedCertDuration: 90, WhitelistedDomains: ["domain.tld"]}
	//   200: success; 403: not an admin
	// PUT /api/config -- update app configuration
	//   I: {ClientLimit: 2, ServiceName: "", IssuedCertDuration: 90, WhitelistedDomains: ["domain.tld"]}
	//   O: {ClientLimit: 2, ServiceName: "", IssuedCertDuration: 90, WhitelistedDomains: ["domain.tld"]}
	//   200: success; 400 (bad request): missing one or more values, or bad values; 403: not an admin
	// non-GET: 405 (method not allowed)

	TAG := "configHandler"

	ssn, s, _, isAdmin := loadSession(req)
	if !ssn.IsLoggedIn() {
		httputil.SendJSON(writer, http.StatusForbidden, &apiResponse{Error: authError})
		return
	}
	if !isAdmin {
		httputil.SendJSON(writer, http.StatusForbidden, &apiResponse{Error: settingsError})
		return
	}

	switch req.Method {
	case "GET":
		httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, s})
	case "PUT":
		if err := httputil.PopulateFromBody(s, req); err != nil {
			httputil.SendJSON(writer, http.StatusBadRequest, &apiResponse{Error: clientJSONError})
			return
		}
		status, err := httputil.CallAPI(httputil.URLJoin(cfg.APIServerURL, "settings"), "PUT", s, s)
		if err != nil {
			panic(err)
		}
		if status >= 300 {
			panic(fmt.Sprintf("non-200 status code %d from API server", status))
		}
		httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, s})
		log.Status(TAG, fmt.Sprintf("settings modified by '%s'", ssn.Email))
	default:
		panic("API method sentinel misconfiguration")
	}
}

func whitelistHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /api/whitelist -- fetch current whitelist
	//   I: none
	//   O: {Users: [""]}
	//   200: success
	// PUT /api/whitelist/<email> -- add a user to the whitelist
	//   I: none
	//   O: {Users: [""]}
	//   200: success; 400: email missing
	// DELETE /api/whitelist/<email> -- delete a user from the whitelist
	//   I: none
	//   O: {Users: [""]}
	//   200: success; 404: email not whitelisted; 400: email missing
	// non-GET: 405 (method not allowed)

	TAG := "whitelistHandler"

	ssn, _, _, isAdmin := loadSession(req)
	if !ssn.IsLoggedIn() {
		httputil.SendJSON(writer, http.StatusForbidden, &apiResponse{Error: authError})
		return
	}
	if !isAdmin {
		httputil.SendJSON(writer, http.StatusForbidden, &apiResponse{Error: settingsError})
		return
	}

	email := extractSegment(req.URL.Path, 3)

	switch req.Method {
	case "GET":
		users := &struct{ Users []string }{}
		status, err := httputil.CallAPI(httputil.URLJoin(cfg.APIServerURL, "whitelist"), "GET", &struct{}{}, users)
		if err != nil {
			panic(err)
		}
		if status >= 300 {
			panic(fmt.Sprintf("non-200 status code %d from API server", status))
		}
		httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, users})
	case "PUT":
		if email == "" {
			httputil.SendJSON(writer, http.StatusBadRequest, apiResponse{Error: clientURLError})
			return
		}
		users := &struct{ Users []string }{}
		status, err := httputil.CallAPI(httputil.URLJoin(cfg.APIServerURL, "whitelist", email), "PUT", &struct{}{}, users)
		if err != nil {
			panic(err)
		}
		if status >= 300 {
			panic(fmt.Sprintf("non-200 status code %d from API server", status))
		}
		log.Status(TAG, fmt.Sprintf("user whitelist updated by '%s'", ssn.Email))
		httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, users})
	case "DELETE":
		if email == "" {
			httputil.SendJSON(writer, http.StatusBadRequest, apiResponse{Error: clientURLError})
			return
		}
		users := &struct{ Users []string }{}
		status, err := httputil.CallAPI(httputil.URLJoin(cfg.APIServerURL, "whitelist", email), "DELETE", &struct{}{}, users)
		if err != nil {
			panic(err)
		}
		if status >= 300 {
			panic(fmt.Sprintf("non-200 status code %d from API server", status))
		}
		log.Status(TAG, fmt.Sprintf("user whitelist updated by '%s'", ssn.Email))
		httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, users})
	default:
		panic("API method sentinel misconfiguration")
	}
}

func usersHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /api/users -- fetch a list of all users with extant certs
	//   I: none
	//   O: {Users: [{Email: "", ActiveCerts: 42, InactiveCerts: 42}]}
	//   200: success
	// GET /api/users/<email> -- fetch a list of a given user's certs
	//   I: none
	//   O: {Email: "", ActiveCerts: [<cert>]}
	//      ...where <cert> == {Fingerprint: "", Description: "", Expires: ""}
	//   200: success; 404: no such email
	// DELETE /api/users/<email> -- revoke all of a user's certs and delete their account
	//   I: none
	//   O: {Email: "", InactiveCerts: 42}
	//   200: success; 404: email not found; 400 (bad request): email missing from request
	// non-GET: 405 (method not allowed)

	TAG := "usersHandler"

	ssn, _, _, isAdmin := loadSession(req)
	if !ssn.IsLoggedIn() {
		httputil.SendJSON(writer, http.StatusForbidden, &apiResponse{Error: authError})
		return
	}
	if !isAdmin {
		httputil.SendJSON(writer, http.StatusForbidden, &apiResponse{Error: usersError})
		return
	}

	email := extractSegment(req.URL.Path, 3)

	switch req.Method {
	case "GET":
		if email == "" {
			type user struct {
				Email       string
				ActiveCerts int
			}
			users := &struct {
				Users []*user
			}{[]*user{}}

			status, err := httputil.CallAPI(httputil.URLJoin(cfg.APIServerURL, "users"), "GET", struct{}{}, users)
			if err != nil {
				panic(err)
			}
			if status >= 300 && status != http.StatusNotFound { // 404 just means no TOTP is set
				panic(fmt.Sprintf("non-200 status code %d from API server", status))
			}

			httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, users})
		} else {
			type cert struct {
				Fingerprint, Expires, Description string
			}
			res := &struct {
				Email, Created string
				ActiveCerts    []*cert
			}{"", "", []*cert{}}

			status, err := httputil.CallAPI(httputil.URLJoin(cfg.APIServerURL, "user", email), "GET", struct{}{}, res)
			if err != nil {
				panic(err)
			}
			if status >= 300 && status != http.StatusNotFound {
				panic(fmt.Sprintf("non-200 status code %d from API server", status))
			}

			for _, c := range res.ActiveCerts {
				if t, err := time.Parse("2006-01-02T15:04:05Z", c.Expires); err != nil {
					panic(err)
				} else {
					c.Expires = t.Format("2006-01-02")
				}
			}

			httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, res})
		}
	case "DELETE":
		status, err := httputil.CallAPI(httputil.URLJoin(cfg.APIServerURL, "user", email), "DELETE", struct{}{}, nil)
		if err != nil {
			panic(err)
		}
		if status >= 300 && status != http.StatusNotFound {
			panic(fmt.Sprintf("non-200 status code %d from API server", status))
		}
		log.Status(TAG, fmt.Sprintf("user '%s' reset by '%s'", email, ssn.Email))
		httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, &struct{ Email string }{email}})
	default:
		panic("API method sentinel misconfiguration")
	}
}

func certsHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /api/certs -- fetch all certs for the current user (i.e. the one making the request)
	//   I: none
	//   O: {Certs: [{Fingerprint: "", Description: "", Expires: ""}]}
	//   200: success
	// POST /api/certs -- create a new client cert
	//   I: {Email: "", Desc: ""}
	//   O: {OVPN: ""}
	//   200: success; 400 (bad request): missing or bad fields;
	//   403: requested email doesn't match session email; 404: Email not known to system (i.e. no TOTP creds)
	//   Note that unless current user is admin, Email is optional but if present must match session email.
	// DELETE /api/certs/<fingerprint> -- fetch details of a client cert
	//   I: none
	//   O: same as GET (above), except that it returns all fingerprints for the user owning the one that was revoked
	//   200: success; 403: session email doesn't own fingerprint and not admin;
	//   404: cert fingerprint not found; 400: fingerprint missing or malformed
	// non-GET: 405 (method not allowed)
	//
	// Note that this handler for /api/certs IS NOT isomorphic with the Heimdall API for certs.
	// Specifically, /api/certs operates on the current user-session's email, and sub-URLs point to
	// specific certs -- i.e. /api/certs/<fingerprint>. Heimdall's API is split into /certs for all
	// users, /certs/<email> for a specific user, and /cert/<fingerprint> for a specific cert.
	TAG := "certsHandler"

	ssn, _, isAllowed, isAdmin := loadSession(req)
	if !ssn.IsLoggedIn() || !isAllowed {
		httputil.SendJSON(writer, http.StatusForbidden, &apiResponse{Error: authError})
		return
	}

	type certMeta struct {
		Fingerprint string
		Description string
		Expires     string
		Created     string `json:",omitEmpty"`
		Revoked     string `json:",omitEmpty"`
	}
	switch req.Method {
	case "GET":
		apiRes := &struct {
			Email, Created            string
			ActiveCerts, RevokedCerts []*certMeta
		}{"", "", []*certMeta{}, []*certMeta{}}

		status, err := httputil.CallAPI(httputil.URLJoin(cfg.APIServerURL, "certs", ssn.Email), "GET", struct{}{}, apiRes)
		if err != nil {
			panic(err)
		}

		if status == http.StatusNotFound {
			// 404 just means no TOTP is set, not fatal
			httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, &struct{ Certs []*certMeta }{[]*certMeta{}}})
			return
		}

		if status >= 300 {
			panic(fmt.Sprintf("non-200 status code %d from API server", status))
		}
		if apiRes.Email != ssn.Email {
			panic(fmt.Sprintf("API server returned wrong email's certs"))
		}

		for _, c := range apiRes.ActiveCerts {
			if t, err := time.Parse("2006-01-02T15:04:05Z", c.Expires); err != nil {
				panic(err)
			} else {
				c.Expires = t.Format("2006-01-02")
			}
		}

		httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, &struct{ Certs []*certMeta }{apiRes.ActiveCerts}})
	case "POST":
		incert := &struct{ Email, Description string }{}

		if err := httputil.PopulateFromBody(incert, req); err != nil {
			httputil.SendJSON(writer, http.StatusBadRequest, apiResponse{Error: clientJSONError})
			return
		}
		if incert.Email != "" && incert.Email != ssn.Email { // not even admins can create certs for other users
			log.Warn(TAG, fmt.Sprintf("'%s' attempted to create cert for '%s'", ssn.Email, incert.Email))
			httputil.SendJSON(writer, http.StatusForbidden, apiResponse{Error: clientJSONError})
			return
		}
		email := incert.Email
		if email == "" {
			email = ssn.Email
		}

		incert.Email = email

		res := &struct{ OVPNDataURL string }{}
		status, err := httputil.CallAPI(httputil.URLJoin(cfg.APIServerURL, "certs", email), "POST", incert, res)
		if err != nil {
			panic(err)
		}
		if status >= 300 {
			panic(fmt.Sprintf("non-200 status code %d from API server", status))
		}
		log.Status(TAG, fmt.Sprintf("'%s' created new certificate '%s'", email, incert.Description))
		httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, res})
	case "DELETE":
		fp := extractSegment(req.URL.Path, 3)
		if fp == "" {
			httputil.SendJSON(writer, http.StatusBadRequest, apiResponse{Error: clientURLError})
			return
		}

		type cert struct {
			Email, Fingerprint, Created, Expires, Revoked, Description string
		}
		apiRes := &cert{}

		// first fetch the metadata for the requested fingerprint to verify ownership
		url := httputil.URLJoin(cfg.APIServerURL, "cert", fp)
		status, err := httputil.CallAPI(url, "GET", struct{}{}, apiRes)
		if err != nil {
			panic(err)
		}
		if status >= 300 {
			panic(fmt.Sprintf("non-200 status code %d from API server", status))
		}
		if apiRes.Email != ssn.Email && !isAdmin {
			log.Warn(TAG, fmt.Sprintf("'%s' attempted to delete '%s' owned by '%s' without admin perms", ssn.Email, fp, apiRes.Email))
			httputil.SendJSON(writer, http.StatusForbidden, &apiResponse{Error: usersError})
			return
		}

		// user is either an admin, or the cert belongs to current user; now do the actual delete
		status, err = httputil.CallAPI(url, "DELETE", struct{}{}, apiRes)
		if err != nil {
			panic(err)
		}
		if status >= 300 {
			panic(fmt.Sprintf("non-200 status code %d from API server", status))
		}

		// ...and finally, fetch the new comprehensive list of certs for the affected user
		getRes := &struct {
			Email, Created            string
			ActiveCerts, RevokedCerts []*certMeta
		}{"", "", []*certMeta{}, []*certMeta{}}
		status, err = httputil.CallAPI(httputil.URLJoin(cfg.APIServerURL, "certs", apiRes.Email), "GET", struct{}{}, getRes)
		if err != nil {
			panic(err)
		}

		if status == http.StatusNotFound {
			// 404 just means no TOTP is set, not fatal
			httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, &struct{ Certs []*certMeta }{[]*certMeta{}}})
			return
		}

		if status >= 300 {
			panic(fmt.Sprintf("non-200 status code %d from API server", status))
		}
		if apiRes.Email != getRes.Email {
			panic(fmt.Sprintf("API server returned wrong email's certs"))
		}

		log.Status(TAG, fmt.Sprintf("'%s' deleted '%s' owned by '%s'", ssn.Email, fp, apiRes.Email))
		httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, &struct{ Certs []*certMeta }{getRes.ActiveCerts}})
	default:
		panic("API method sentinel misconfiguration")
	}
}

func totpHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /api/totp -- returns whether the current user has TOTP configured
	//   I: none
	//   O: {Configured: false}
	//   200: success
	// POST /api/totp -- generate a new TOTP seed for the current user
	//   I: none
	//   O: {ImageURL: ""}
	//   200: success; 400 (bad request): missing or bad fields;
	// non-GET: 405 (method not allowed)
	//
	// Note that this endpoint handles ONLY TOTP (re)generation for the current user. Deletion of other
	// users by admins is handled via the /api/users/ endpoint.
	TAG := "totpHandler"

	ssn, _, isAllowed, _ := loadSession(req)
	if !ssn.IsLoggedIn() || !isAllowed {
		httputil.SendJSON(writer, http.StatusForbidden, &apiResponse{Error: authError})
		return
	}

	url := httputil.URLJoin(cfg.APIServerURL, "user", ssn.Email)

	switch req.Method {
	case "GET":
		configured := &struct{ Configured bool }{}
		res := &struct{ Email string }{} // this API call has more fields but we only care about this, here

		status, err := httputil.CallAPI(url, "GET", struct{}{}, res)
		if err != nil {
			panic(err)
		}
		if status == 404 {
			// not fatal -- just means the user has no TOTP set
			configured.Configured = false
			httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, configured})
		} else if status <= 299 {
			if res.Email != ssn.Email {
				panic("API server returned results for wrong user")
			}
			configured.Configured = true
			httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, configured})
		} else {
			panic(fmt.Sprintf("non-200 status code %d from API server", status))
		}
	case "POST":
		set := &struct{ ImageURL string }{}
		res := &struct{ Email, TOTPURL string }{}

		status, err := httputil.CallAPI(url, "PUT", struct{}{}, res)
		if err != nil {
			panic(err)
		}
		if status <= 299 {
			if res.Email != ssn.Email {
				panic("API server returned results for wrong user")
			}
			set.ImageURL = res.TOTPURL
			log.Status(TAG, fmt.Sprintf("'%s' set TOTP seed", ssn.Email))
			httputil.SendJSON(writer, http.StatusOK, apiResponse{nil, set})
		} else {
			panic(fmt.Sprintf("non-200 status code %d from API server", status))
		}
	default:
		panic("API method sentinel misconfiguration")
	}
}

func eventsHandler(writer http.ResponseWriter, req *http.Request) {
	// GET /api/events -- returns whether the current user has TOTP configured
	//   I: none
	//   O: {Events: [{Event: "", Email: "", Value: "", Timestamp: ""}]}
	//   200: success
	// non-GET: 405 (method not allowed)
	// Accepts a GET query parameter of "?before=" which is passed to the API server, for pagination
	// If the value is "all", returns everything (i.e. a dump/export)

	ssn, _, _, isAdmin := loadSession(req)
	if !ssn.IsLoggedIn() {
		httputil.SendJSON(writer, http.StatusForbidden, &apiResponse{Error: authError})
		return
	}
	if !isAdmin {
		httputil.SendJSON(writer, http.StatusForbidden, &apiResponse{Error: eventsError})
		return
	}

	type event struct{ Event, Email, Value, Timestamp string }
	res := &struct{ Events []*event }{}

	if err := req.ParseForm(); err != nil {
		panic(err)
	}
	before := req.FormValue("before")

	// fish out a ?before= pagination param and pass it on to API server if present
	u := httputil.URLJoin(cfg.APIServerURL, "events")
	if before != "" {
		v := url.Values{}
		v.Add("before", before)
		parsed, err := url.Parse(u)
		if err != nil {
			panic(err)
		}
		parsed.RawQuery = v.Encode()
		u = parsed.String()
	}
	status, err := httputil.CallAPI(u, "GET", struct{}{}, res)
	if err != nil {
		panic(err)
	}
	if status > 299 {
		panic(fmt.Sprintf("non-200 status code %d from API server", status))
	}

	httputil.SendJSON(writer, http.StatusOK, &apiResponse{Artifact: res})
}