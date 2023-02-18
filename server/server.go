package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/emersion/go-smtp"
	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"
	"heckel.io/ntfy/log"
	"heckel.io/ntfy/user"
	"heckel.io/ntfy/util"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Server is the main server, providing the UI and API for ntfy
type Server struct {
	config            *Config
	httpServer        *http.Server
	httpsServer       *http.Server
	unixListener      net.Listener
	smtpServer        *smtp.Server
	smtpServerBackend *smtpBackend
	smtpSender        mailer
	topics            map[string]*topic
	visitors          map[string]*visitor // ip:<ip> or user:<user>
	firebaseClient    *firebaseClient
	messages          int64
	userManager       *user.Manager                        // Might be nil!
	messageCache      *messageCache                        // Database that stores the messages
	fileCache         *fileCache                           // File system based cache that stores attachments
	stripe            stripeAPI                            // Stripe API, can be replaced with a mock
	priceCache        *util.LookupCache[map[string]string] // Stripe price ID -> formatted price
	closeChan         chan bool
	mu                sync.Mutex
}

// handleFunc extends the normal http.HandlerFunc to be able to easily return errors
type handleFunc func(http.ResponseWriter, *http.Request, *visitor) error

var (
	// If changed, don't forget to update Android App and auth_sqlite.go
	topicRegex             = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)               // No /!
	topicPathRegex         = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}$`)              // Regex must match JS & Android app!
	externalTopicPathRegex = regexp.MustCompile(`^/[^/]+\.[^/]+/[-_A-Za-z0-9]{1,64}$`) // Extended topic path, for web-app, e.g. /example.com/mytopic
	jsonPathRegex          = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/json$`)
	ssePathRegex           = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/sse$`)
	rawPathRegex           = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/raw$`)
	wsPathRegex            = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/ws$`)
	authPathRegex          = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}(,[-_A-Za-z0-9]{1,64})*/auth$`)
	publishPathRegex       = regexp.MustCompile(`^/[-_A-Za-z0-9]{1,64}/(publish|send|trigger)$`)

	webConfigPath                                        = "/config.js"
	accountPath                                          = "/account"
	matrixPushPath                                       = "/_matrix/push/v1/notify"
	apiHealthPath                                        = "/v1/health"
	apiTiers                                             = "/v1/tiers"
	apiAccountPath                                       = "/v1/account"
	apiAccountTokenPath                                  = "/v1/account/token"
	apiAccountPasswordPath                               = "/v1/account/password"
	apiAccountSettingsPath                               = "/v1/account/settings"
	apiAccountSubscriptionPath                           = "/v1/account/subscription"
	apiAccountReservationPath                            = "/v1/account/reservation"
	apiAccountBillingPortalPath                          = "/v1/account/billing/portal"
	apiAccountBillingWebhookPath                         = "/v1/account/billing/webhook"
	apiAccountBillingSubscriptionPath                    = "/v1/account/billing/subscription"
	apiAccountBillingSubscriptionCheckoutSuccessTemplate = "/v1/account/billing/subscription/success/{CHECKOUT_SESSION_ID}"
	apiAccountBillingSubscriptionCheckoutSuccessRegex    = regexp.MustCompile(`/v1/account/billing/subscription/success/(.+)$`)
	apiAccountReservationSingleRegex                     = regexp.MustCompile(`/v1/account/reservation/([-_A-Za-z0-9]{1,64})$`)
	staticRegex                                          = regexp.MustCompile(`^/static/.+`)
	docsRegex                                            = regexp.MustCompile(`^/docs(|/.*)$`)
	fileRegex                                            = regexp.MustCompile(`^/file/([-_A-Za-z0-9]{1,64})(?:\.[A-Za-z0-9]{1,16})?$`)
	urlRegex                                             = regexp.MustCompile(`^https?://`)

	//go:embed site
	webFs        embed.FS
	webFsCached  = &util.CachingEmbedFS{ModTime: time.Now(), FS: webFs}
	webSiteDir   = "/site"
	webHomeIndex = "/home.html" // Landing page, only if "web-root: home"
	webAppIndex  = "/app.html"  // React app

	//go:embed docs
	docsStaticFs     embed.FS
	docsStaticCached = &util.CachingEmbedFS{ModTime: time.Now(), FS: docsStaticFs}
)

const (
	firebaseControlTopic     = "~control"                // See Android if changed
	firebasePollTopic        = "~poll"                   // See iOS if changed
	emptyMessageBody         = "triggered"               // Used if message body is empty
	newMessageBody           = "New message"             // Used in poll requests as generic message
	defaultAttachmentMessage = "You received a file: %s" // Used if message body is empty, and there is an attachment
	encodingBase64           = "base64"                  // Used mainly for binary UnifiedPush messages
	jsonBodyBytesLimit       = 16384
)

// WebSocket constants
const (
	wsWriteWait  = 2 * time.Second
	wsBufferSize = 1024
	wsReadLimit  = 64 // We only ever receive PINGs
	wsPongWait   = 15 * time.Second
)

// New instantiates a new Server. It creates the cache and adds a Firebase
// subscriber (if configured).
func New(conf *Config) (*Server, error) {
	var mailer mailer
	if conf.SMTPSenderAddr != "" {
		mailer = &smtpSender{config: conf}
	}
	var stripe stripeAPI
	if conf.StripeSecretKey != "" {
		stripe = newStripeAPI()
	}
	messageCache, err := createMessageCache(conf)
	if err != nil {
		return nil, err
	}
	topics, err := messageCache.Topics()
	if err != nil {
		return nil, err
	}
	var fileCache *fileCache
	if conf.AttachmentCacheDir != "" {
		fileCache, err = newFileCache(conf.AttachmentCacheDir, conf.AttachmentTotalSizeLimit)
		if err != nil {
			return nil, err
		}
	}
	var userManager *user.Manager
	if conf.AuthFile != "" {
		userManager, err = user.NewManager(conf.AuthFile, conf.AuthStartupQueries, conf.AuthDefault, conf.AuthBcryptCost, conf.AuthStatsQueueWriterInterval)
		if err != nil {
			return nil, err
		}
	}
	var firebaseClient *firebaseClient
	if conf.FirebaseKeyFile != "" {
		sender, err := newFirebaseSender(conf.FirebaseKeyFile)
		if err != nil {
			return nil, err
		}
		firebaseClient = newFirebaseClient(sender, userManager)
	}
	s := &Server{
		config:         conf,
		messageCache:   messageCache,
		fileCache:      fileCache,
		firebaseClient: firebaseClient,
		smtpSender:     mailer,
		topics:         topics,
		userManager:    userManager,
		visitors:       make(map[string]*visitor),
		stripe:         stripe,
	}
	s.priceCache = util.NewLookupCache(s.fetchStripePrices, conf.StripePriceCacheDuration)
	return s, nil
}

func createMessageCache(conf *Config) (*messageCache, error) {
	if conf.CacheDuration == 0 {
		return newNopCache()
	} else if conf.CacheFile != "" {
		return newSqliteCache(conf.CacheFile, conf.CacheStartupQueries, conf.CacheDuration, conf.CacheBatchSize, conf.CacheBatchTimeout, false)
	}
	return newMemCache()
}

// Run executes the main server. It listens on HTTP (+ HTTPS, if configured), and starts
// a manager go routine to print stats and prune messages.
func (s *Server) Run() error {
	var listenStr string
	if s.config.ListenHTTP != "" {
		listenStr += fmt.Sprintf(" %s[http]", s.config.ListenHTTP)
	}
	if s.config.ListenHTTPS != "" {
		listenStr += fmt.Sprintf(" %s[https]", s.config.ListenHTTPS)
	}
	if s.config.ListenUnix != "" {
		listenStr += fmt.Sprintf(" %s[unix]", s.config.ListenUnix)
	}
	if s.config.SMTPServerListen != "" {
		listenStr += fmt.Sprintf(" %s[smtp]", s.config.SMTPServerListen)
	}
	log.Tag(tagStartup).Info("Listening on%s, ntfy %s, log level is %s", listenStr, s.config.Version, log.CurrentLevel().String())
	if log.IsFile() {
		fmt.Fprintf(os.Stderr, "Listening on%s, ntfy %s\n", listenStr, s.config.Version)
		fmt.Fprintf(os.Stderr, "Logs are written to %s\n", log.File())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	errChan := make(chan error)
	s.mu.Lock()
	s.closeChan = make(chan bool)
	if s.config.ListenHTTP != "" {
		s.httpServer = &http.Server{Addr: s.config.ListenHTTP, Handler: mux}
		go func() {
			errChan <- s.httpServer.ListenAndServe()
		}()
	}
	if s.config.ListenHTTPS != "" {
		s.httpsServer = &http.Server{Addr: s.config.ListenHTTPS, Handler: mux}
		go func() {
			errChan <- s.httpsServer.ListenAndServeTLS(s.config.CertFile, s.config.KeyFile)
		}()
	}
	if s.config.ListenUnix != "" {
		go func() {
			var err error
			s.mu.Lock()
			os.Remove(s.config.ListenUnix)
			s.unixListener, err = net.Listen("unix", s.config.ListenUnix)
			if err != nil {
				s.mu.Unlock()
				errChan <- err
				return
			}
			defer s.unixListener.Close()
			if s.config.ListenUnixMode > 0 {
				if err := os.Chmod(s.config.ListenUnix, s.config.ListenUnixMode); err != nil {
					s.mu.Unlock()
					errChan <- err
					return
				}
			}
			s.mu.Unlock()
			httpServer := &http.Server{Handler: mux}
			errChan <- httpServer.Serve(s.unixListener)
		}()
	}
	if s.config.SMTPServerListen != "" {
		go func() {
			errChan <- s.runSMTPServer()
		}()
	}
	s.mu.Unlock()
	go s.runManager()
	go s.runStatsResetter()
	go s.runDelayedSender()
	go s.runFirebaseKeepaliver()

	return <-errChan
}

// Stop stops HTTP (+HTTPS) server and all managers
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpServer != nil {
		s.httpServer.Close()
	}
	if s.httpsServer != nil {
		s.httpsServer.Close()
	}
	if s.unixListener != nil {
		s.unixListener.Close()
	}
	if s.smtpServer != nil {
		s.smtpServer.Close()
	}
	s.closeDatabases()
	close(s.closeChan)
}

func (s *Server) closeDatabases() {
	if s.userManager != nil {
		s.userManager.Close()
	}
	s.messageCache.Close()
}

// handle is the main entry point for all HTTP requests
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	v, err := s.maybeAuthenticate(r) // Note: Always returns v, even when error is returned
	if err != nil {
		s.handleError(w, r, v, err)
		return
	}
	ev := logvr(v, r)
	if ev.IsTrace() {
		ev.Field("http_request", renderHTTPRequest(r)).Trace("HTTP request started")
	} else if logvr(v, r).IsDebug() {
		ev.Debug("HTTP request started")
	}
	logvr(v, r).
		Timing(func() {
			if err := s.handleInternal(w, r, v); err != nil {
				s.handleError(w, r, v, err)
				return
			}
		}).
		Debug("HTTP request finished")
}

func (s *Server) handleError(w http.ResponseWriter, r *http.Request, v *visitor, err error) {
	httpErr, ok := err.(*errHTTP)
	if !ok {
		httpErr = errHTTPInternalError
	}
	isNormalError := strings.Contains(err.Error(), "i/o timeout") || util.Contains([]int{http.StatusNotFound, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusUnauthorized}, httpErr.HTTPCode)
	if websocket.IsWebSocketUpgrade(r) {
		if isNormalError {
			logvr(v, r).Tag(tagWebsocket).Err(err).Fields(websocketErrorContext(err)).Debug("WebSocket error (this error is okay, it happens a lot): %s", err.Error())
		} else {
			logvr(v, r).Tag(tagWebsocket).Err(err).Fields(websocketErrorContext(err)).Info("WebSocket error: %s", err.Error())
		}
		return // Do not attempt to write to upgraded connection
	}
	if matrixErr, ok := err.(*errMatrix); ok {
		if err := writeMatrixError(w, r, v, matrixErr); err != nil {
			logvr(v, r).Tag(tagMatrix).Err(err).Debug("Writing Matrix error failed")
		}
		return
	}
	if isNormalError {
		logvr(v, r).Err(err).Debug("Connection closed with HTTP %d (ntfy error %d)", httpErr.HTTPCode, httpErr.Code)
	} else {
		logvr(v, r).Err(err).Info("Connection closed with HTTP %d (ntfy error %d)", httpErr.HTTPCode, httpErr.Code)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", s.config.AccessControlAllowOrigin) // CORS, allow cross-origin requests
	w.WriteHeader(httpErr.HTTPCode)
	io.WriteString(w, httpErr.JSON()+"\n")
}

func (s *Server) handleInternal(w http.ResponseWriter, r *http.Request, v *visitor) error {
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		return s.ensureWebEnabled(s.handleHome)(w, r, v)
	} else if r.Method == http.MethodHead && r.URL.Path == "/" {
		return s.ensureWebEnabled(s.handleEmpty)(w, r, v)
	} else if r.Method == http.MethodGet && r.URL.Path == apiHealthPath {
		return s.handleHealth(w, r, v)
	} else if r.Method == http.MethodGet && r.URL.Path == webConfigPath {
		return s.ensureWebEnabled(s.handleWebConfig)(w, r, v)
	} else if r.Method == http.MethodPost && r.URL.Path == apiAccountPath {
		return s.ensureUserManager(s.handleAccountCreate)(w, r, v)
	} else if r.Method == http.MethodGet && r.URL.Path == apiAccountPath {
		return s.handleAccountGet(w, r, v) // Allowed by anonymous
	} else if r.Method == http.MethodDelete && r.URL.Path == apiAccountPath {
		return s.ensureUser(s.withAccountSync(s.handleAccountDelete))(w, r, v)
	} else if r.Method == http.MethodPost && r.URL.Path == apiAccountPasswordPath {
		return s.ensureUser(s.handleAccountPasswordChange)(w, r, v)
	} else if r.Method == http.MethodPost && r.URL.Path == apiAccountTokenPath {
		return s.ensureUser(s.withAccountSync(s.handleAccountTokenCreate))(w, r, v)
	} else if r.Method == http.MethodPatch && r.URL.Path == apiAccountTokenPath {
		return s.ensureUser(s.withAccountSync(s.handleAccountTokenUpdate))(w, r, v)
	} else if r.Method == http.MethodDelete && r.URL.Path == apiAccountTokenPath {
		return s.ensureUser(s.withAccountSync(s.handleAccountTokenDelete))(w, r, v)
	} else if r.Method == http.MethodPatch && r.URL.Path == apiAccountSettingsPath {
		return s.ensureUser(s.withAccountSync(s.handleAccountSettingsChange))(w, r, v)
	} else if r.Method == http.MethodPost && r.URL.Path == apiAccountSubscriptionPath {
		return s.ensureUser(s.withAccountSync(s.handleAccountSubscriptionAdd))(w, r, v)
	} else if r.Method == http.MethodPatch && r.URL.Path == apiAccountSubscriptionPath {
		return s.ensureUser(s.withAccountSync(s.handleAccountSubscriptionChange))(w, r, v)
	} else if r.Method == http.MethodDelete && r.URL.Path == apiAccountSubscriptionPath {
		return s.ensureUser(s.withAccountSync(s.handleAccountSubscriptionDelete))(w, r, v)
	} else if r.Method == http.MethodPost && r.URL.Path == apiAccountReservationPath {
		return s.ensureUser(s.withAccountSync(s.handleAccountReservationAdd))(w, r, v)
	} else if r.Method == http.MethodDelete && apiAccountReservationSingleRegex.MatchString(r.URL.Path) {
		return s.ensureUser(s.withAccountSync(s.handleAccountReservationDelete))(w, r, v)
	} else if r.Method == http.MethodPost && r.URL.Path == apiAccountBillingSubscriptionPath {
		return s.ensurePaymentsEnabled(s.ensureUser(s.handleAccountBillingSubscriptionCreate))(w, r, v) // Account sync via incoming Stripe webhook
	} else if r.Method == http.MethodGet && apiAccountBillingSubscriptionCheckoutSuccessRegex.MatchString(r.URL.Path) {
		return s.ensurePaymentsEnabled(s.ensureUserManager(s.handleAccountBillingSubscriptionCreateSuccess))(w, r, v) // No user context!
	} else if r.Method == http.MethodPut && r.URL.Path == apiAccountBillingSubscriptionPath {
		return s.ensurePaymentsEnabled(s.ensureStripeCustomer(s.handleAccountBillingSubscriptionUpdate))(w, r, v) // Account sync via incoming Stripe webhook
	} else if r.Method == http.MethodDelete && r.URL.Path == apiAccountBillingSubscriptionPath {
		return s.ensurePaymentsEnabled(s.ensureStripeCustomer(s.handleAccountBillingSubscriptionDelete))(w, r, v) // Account sync via incoming Stripe webhook
	} else if r.Method == http.MethodPost && r.URL.Path == apiAccountBillingPortalPath {
		return s.ensurePaymentsEnabled(s.ensureStripeCustomer(s.handleAccountBillingPortalSessionCreate))(w, r, v)
	} else if r.Method == http.MethodPost && r.URL.Path == apiAccountBillingWebhookPath {
		return s.ensurePaymentsEnabled(s.ensureUserManager(s.handleAccountBillingWebhook))(w, r, v) // This request comes from Stripe!
	} else if r.Method == http.MethodGet && r.URL.Path == apiTiers {
		return s.ensurePaymentsEnabled(s.handleBillingTiersGet)(w, r, v)
	} else if r.Method == http.MethodGet && r.URL.Path == matrixPushPath {
		return s.handleMatrixDiscovery(w)
	} else if r.Method == http.MethodGet && staticRegex.MatchString(r.URL.Path) {
		return s.ensureWebEnabled(s.handleStatic)(w, r, v)
	} else if r.Method == http.MethodGet && docsRegex.MatchString(r.URL.Path) {
		return s.ensureWebEnabled(s.handleDocs)(w, r, v)
	} else if (r.Method == http.MethodGet || r.Method == http.MethodHead) && fileRegex.MatchString(r.URL.Path) && s.config.AttachmentCacheDir != "" {
		return s.limitRequests(s.handleFile)(w, r, v)
	} else if r.Method == http.MethodOptions {
		return s.limitRequests(s.handleOptions)(w, r, v) // Should work even if the web app is not enabled, see #598
	} else if (r.Method == http.MethodPut || r.Method == http.MethodPost) && r.URL.Path == "/" {
		return s.limitRequests(s.transformBodyJSON(s.authorizeTopicWrite(s.handlePublish)))(w, r, v)
	} else if r.Method == http.MethodPost && r.URL.Path == matrixPushPath {
		return s.limitRequests(s.transformMatrixJSON(s.authorizeTopicWrite(s.handlePublishMatrix)))(w, r, v)
	} else if (r.Method == http.MethodPut || r.Method == http.MethodPost) && topicPathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authorizeTopicWrite(s.handlePublish))(w, r, v)
	} else if r.Method == http.MethodGet && publishPathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authorizeTopicWrite(s.handlePublish))(w, r, v)
	} else if r.Method == http.MethodGet && jsonPathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authorizeTopicRead(s.handleSubscribeJSON))(w, r, v)
	} else if r.Method == http.MethodGet && ssePathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authorizeTopicRead(s.handleSubscribeSSE))(w, r, v)
	} else if r.Method == http.MethodGet && rawPathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authorizeTopicRead(s.handleSubscribeRaw))(w, r, v)
	} else if r.Method == http.MethodGet && wsPathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authorizeTopicRead(s.handleSubscribeWS))(w, r, v)
	} else if r.Method == http.MethodGet && authPathRegex.MatchString(r.URL.Path) {
		return s.limitRequests(s.authorizeTopicRead(s.handleTopicAuth))(w, r, v)
	} else if r.Method == http.MethodGet && (topicPathRegex.MatchString(r.URL.Path) || externalTopicPathRegex.MatchString(r.URL.Path)) {
		return s.ensureWebEnabled(s.handleTopic)(w, r, v)
	}
	return errHTTPNotFound
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request, v *visitor) error {
	if s.config.WebRootIsApp {
		r.URL.Path = webAppIndex
	} else {
		r.URL.Path = webHomeIndex
	}
	return s.handleStatic(w, r, v)
}

func (s *Server) handleTopic(w http.ResponseWriter, r *http.Request, v *visitor) error {
	unifiedpush := readBoolParam(r, false, "x-unifiedpush", "unifiedpush", "up") // see PUT/POST too!
	if unifiedpush {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", s.config.AccessControlAllowOrigin) // CORS, allow cross-origin requests
		_, err := io.WriteString(w, `{"unifiedpush":{"version":1}}`+"\n")
		return err
	}
	r.URL.Path = webAppIndex
	return s.handleStatic(w, r, v)
}

func (s *Server) handleEmpty(_ http.ResponseWriter, _ *http.Request, _ *visitor) error {
	return nil
}

func (s *Server) handleTopicAuth(w http.ResponseWriter, _ *http.Request, _ *visitor) error {
	return s.writeJSON(w, newSuccessResponse())
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request, _ *visitor) error {
	response := &apiHealthResponse{
		Healthy: true,
	}
	return s.writeJSON(w, response)
}

func (s *Server) handleWebConfig(w http.ResponseWriter, _ *http.Request, _ *visitor) error {
	appRoot := "/"
	if !s.config.WebRootIsApp {
		appRoot = "/app"
	}
	response := &apiConfigResponse{
		BaseURL:            "", // Will translate to window.location.origin
		AppRoot:            appRoot,
		EnableLogin:        s.config.EnableLogin,
		EnableSignup:       s.config.EnableSignup,
		EnablePayments:     s.config.StripeSecretKey != "",
		EnableReservations: s.config.EnableReservations,
		DisallowedTopics:   s.config.DisallowedTopics,
	}
	b, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/javascript")
	_, err = io.WriteString(w, fmt.Sprintf("// Generated server configuration\nvar config = %s;\n", string(b)))
	return err
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request, _ *visitor) error {
	r.URL.Path = webSiteDir + r.URL.Path
	util.Gzip(http.FileServer(http.FS(webFsCached))).ServeHTTP(w, r)
	return nil
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request, _ *visitor) error {
	util.Gzip(http.FileServer(http.FS(docsStaticCached))).ServeHTTP(w, r)
	return nil
}

// handleFile processes the download of attachment files. The method handles GET and HEAD requests against a file.
// Before streaming the file to a client, it locates uploader (m.Sender or m.User) in the message cache, so it
// can associate the download bandwidth with the uploader.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request, v *visitor) error {
	if s.config.AttachmentCacheDir == "" {
		return errHTTPInternalError
	}
	matches := fileRegex.FindStringSubmatch(r.URL.Path)
	if len(matches) != 2 {
		return errHTTPInternalErrorInvalidPath
	}
	messageID := matches[1]
	file := filepath.Join(s.config.AttachmentCacheDir, messageID)
	stat, err := os.Stat(file)
	if err != nil {
		return errHTTPNotFound
	}
	w.Header().Set("Access-Control-Allow-Origin", s.config.AccessControlAllowOrigin) // CORS, allow cross-origin requests
	w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
	if r.Method == http.MethodHead {
		return nil
	}
	// Find message in database, and associate bandwidth to the uploader user
	// This is an easy way to
	//   - avoid abuse (e.g. 1 uploader, 1k downloaders)
	//   - and also uses the higher bandwidth limits of a paying user
	m, err := s.messageCache.Message(messageID)
	if err == errMessageNotFound {
		if s.config.CacheBatchTimeout > 0 {
			// Strange edge case: If we immediately after upload request the file (the web app does this for images),
			// and messages are persisted asynchronously, retry fetching from the database
			m, err = util.Retry(func() (*message, error) {
				return s.messageCache.Message(messageID)
			}, s.config.CacheBatchTimeout, 100*time.Millisecond, 300*time.Millisecond, 600*time.Millisecond)
		}
		if err != nil {
			return errHTTPNotFound
		}
	} else if err != nil {
		return err
	}
	bandwidthVisitor := v
	if s.userManager != nil && m.User != "" {
		u, err := s.userManager.UserByID(m.User)
		if err != nil {
			return err
		}
		bandwidthVisitor = s.visitor(v.IP(), u)
	} else if m.Sender.IsValid() {
		bandwidthVisitor = s.visitor(m.Sender, nil)
	}
	if !bandwidthVisitor.BandwidthAllowed(stat.Size()) {
		return errHTTPTooManyRequestsLimitAttachmentBandwidth
	}
	// Actually send file
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(util.NewContentTypeWriter(w, r.URL.Path), f)
	return err
}

func (s *Server) handleMatrixDiscovery(w http.ResponseWriter) error {
	if s.config.BaseURL == "" {
		return errHTTPInternalErrorMissingBaseURL
	}
	return writeMatrixDiscoveryResponse(w)
}

func (s *Server) handlePublishWithoutResponse(r *http.Request, v *visitor) (*message, error) {
	t, err := s.topicFromPath(r.URL.Path)
	if err != nil {
		return nil, err
	}
	if !v.MessageAllowed() {
		return nil, errHTTPTooManyRequestsLimitMessages
	}
	body, err := util.Peek(r.Body, s.config.MessageLimit)
	if err != nil {
		return nil, err
	}
	m := newDefaultMessage(t.ID, "")
	cache, firebase, email, unifiedpush, err := s.parsePublishParams(r, v, m)
	if err != nil {
		return nil, err
	}
	if m.PollID != "" {
		m = newPollRequestMessage(t.ID, m.PollID)
	}
	m.Sender = v.IP()
	m.User = v.MaybeUserID()
	m.Expires = time.Unix(m.Time, 0).Add(v.Limits().MessageExpiryDuration).Unix()
	if err := s.handlePublishBody(r, v, m, body, unifiedpush); err != nil {
		return nil, err
	}
	if m.Message == "" {
		m.Message = emptyMessageBody
	}
	delayed := m.Time > time.Now().Unix()
	ev := logvrm(v, r, m).
		Tag(tagPublish).
		Fields(log.Context{
			"message_delayed":     delayed,
			"message_firebase":    firebase,
			"message_unifiedpush": unifiedpush,
			"message_email":       email,
		})
	if ev.IsTrace() {
		ev.Field("message_body", util.MaybeMarshalJSON(m)).Trace("Received message")
	} else if ev.IsDebug() {
		ev.Debug("Received message")
	}
	if !delayed {
		if err := t.Publish(v, m); err != nil {
			return nil, err
		}
		if s.firebaseClient != nil && firebase {
			go s.sendToFirebase(v, m)
		}
		if s.smtpSender != nil && email != "" {
			go s.sendEmail(v, m, email)
		}
		if s.config.UpstreamBaseURL != "" {
			go s.forwardPollRequest(v, m)
		}
	} else {
		logvrm(v, r, m).Tag(tagPublish).Debug("Message delayed, will process later")
	}
	if cache {
		logvrm(v, r, m).Tag(tagPublish).Debug("Adding message to cache")
		if err := s.messageCache.AddMessage(m); err != nil {
			return nil, err
		}
	}
	u := v.User()
	if s.userManager != nil && u != nil && u.Tier != nil {
		go s.userManager.EnqueueUserStats(u.ID, v.Stats())
	}
	s.mu.Lock()
	s.messages++
	s.mu.Unlock()
	return m, nil
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request, v *visitor) error {
	m, err := s.handlePublishWithoutResponse(r, v)
	if err != nil {
		return err
	}
	return s.writeJSON(w, m)
}

func (s *Server) handlePublishMatrix(w http.ResponseWriter, r *http.Request, v *visitor) error {
	_, err := s.handlePublishWithoutResponse(r, v)
	if err != nil {
		return &errMatrix{pushKey: r.Header.Get(matrixPushKeyHeader), err: err}
	}
	return writeMatrixSuccess(w)
}

func (s *Server) sendToFirebase(v *visitor, m *message) {
	logvm(v, m).Tag(tagFirebase).Debug("Publishing to Firebase")
	if err := s.firebaseClient.Send(v, m); err != nil {
		if err == errFirebaseTemporarilyBanned {
			logvm(v, m).Tag(tagFirebase).Err(err).Debug("Unable to publish to Firebase: %v", err.Error())
		} else {
			logvm(v, m).Tag(tagFirebase).Err(err).Warn("Unable to publish to Firebase: %v", err.Error())
		}
	}
}

func (s *Server) sendEmail(v *visitor, m *message, email string) {
	logvm(v, m).Tag(tagEmail).Field("email", email).Debug("Sending email to %s", email)
	if err := s.smtpSender.Send(v, m, email); err != nil {
		logvm(v, m).Tag(tagEmail).Field("email", email).Err(err).Warn("Unable to send email to %s: %v", email, err.Error())
	}
}

func (s *Server) forwardPollRequest(v *visitor, m *message) {
	topicURL := fmt.Sprintf("%s/%s", s.config.BaseURL, m.Topic)
	topicHash := fmt.Sprintf("%x", sha256.Sum256([]byte(topicURL)))
	forwardURL := fmt.Sprintf("%s/%s", s.config.UpstreamBaseURL, topicHash)
	logvm(v, m).Debug("Publishing poll request to %s", forwardURL)
	req, err := http.NewRequest("POST", forwardURL, strings.NewReader(""))
	if err != nil {
		logvm(v, m).Err(err).Warn("Unable to publish poll request")
		return
	}
	req.Header.Set("X-Poll-ID", m.ID)
	var httpClient = &http.Client{
		Timeout: time.Second * 10,
	}
	response, err := httpClient.Do(req)
	if err != nil {
		logvm(v, m).Err(err).Warn("Unable to publish poll request")
		return
	} else if response.StatusCode != http.StatusOK {
		logvm(v, m).Err(err).Warn("Unable to publish poll request, unexpected HTTP status: %d", response.StatusCode)
		return
	}
}

func (s *Server) parsePublishParams(r *http.Request, v *visitor, m *message) (cache bool, firebase bool, email string, unifiedpush bool, err error) {
	cache = readBoolParam(r, true, "x-cache", "cache")
	firebase = readBoolParam(r, true, "x-firebase", "firebase")
	m.Title = readParam(r, "x-title", "title", "t")
	m.Click = readParam(r, "x-click", "click")
	icon := readParam(r, "x-icon", "icon")
	filename := readParam(r, "x-filename", "filename", "file", "f")
	attach := readParam(r, "x-attach", "attach", "a")
	if attach != "" || filename != "" {
		m.Attachment = &attachment{}
	}
	if filename != "" {
		m.Attachment.Name = filename
	}
	if attach != "" {
		if !urlRegex.MatchString(attach) {
			return false, false, "", false, errHTTPBadRequestAttachmentURLInvalid
		}
		m.Attachment.URL = attach
		if m.Attachment.Name == "" {
			u, err := url.Parse(m.Attachment.URL)
			if err == nil {
				m.Attachment.Name = path.Base(u.Path)
				if m.Attachment.Name == "." || m.Attachment.Name == "/" {
					m.Attachment.Name = ""
				}
			}
		}
		if m.Attachment.Name == "" {
			m.Attachment.Name = "attachment"
		}
	}
	if icon != "" {
		if !urlRegex.MatchString(icon) {
			return false, false, "", false, errHTTPBadRequestIconURLInvalid
		}
		m.Icon = icon
	}
	email = readParam(r, "x-email", "x-e-mail", "email", "e-mail", "mail", "e")
	if email != "" {
		if !v.EmailAllowed() {
			return false, false, "", false, errHTTPTooManyRequestsLimitEmails
		}
	}
	if s.smtpSender == nil && email != "" {
		return false, false, "", false, errHTTPBadRequestEmailDisabled
	}
	messageStr := strings.ReplaceAll(readParam(r, "x-message", "message", "m"), "\\n", "\n")
	if messageStr != "" {
		m.Message = messageStr
	}
	m.Priority, err = util.ParsePriority(readParam(r, "x-priority", "priority", "prio", "p"))
	if err != nil {
		return false, false, "", false, errHTTPBadRequestPriorityInvalid
	}
	tagsStr := readParam(r, "x-tags", "tags", "tag", "ta")
	if tagsStr != "" {
		m.Tags = make([]string, 0)
		for _, s := range util.SplitNoEmpty(tagsStr, ",") {
			m.Tags = append(m.Tags, strings.TrimSpace(s))
		}
	}
	delayStr := readParam(r, "x-delay", "delay", "x-at", "at", "x-in", "in")
	if delayStr != "" {
		if !cache {
			return false, false, "", false, errHTTPBadRequestDelayNoCache
		}
		if email != "" {
			return false, false, "", false, errHTTPBadRequestDelayNoEmail // we cannot store the email address (yet)
		}
		delay, err := util.ParseFutureTime(delayStr, time.Now())
		if err != nil {
			return false, false, "", false, errHTTPBadRequestDelayCannotParse
		} else if delay.Unix() < time.Now().Add(s.config.MinDelay).Unix() {
			return false, false, "", false, errHTTPBadRequestDelayTooSmall
		} else if delay.Unix() > time.Now().Add(s.config.MaxDelay).Unix() {
			return false, false, "", false, errHTTPBadRequestDelayTooLarge
		}
		m.Time = delay.Unix()
	}
	actionsStr := readParam(r, "x-actions", "actions", "action")
	if actionsStr != "" {
		m.Actions, err = parseActions(actionsStr)
		if err != nil {
			return false, false, "", false, wrapErrHTTP(errHTTPBadRequestActionsInvalid, err.Error())
		}
	}
	unifiedpush = readBoolParam(r, false, "x-unifiedpush", "unifiedpush", "up") // see GET too!
	if unifiedpush {
		firebase = false
		unifiedpush = true
	}
	m.PollID = readParam(r, "x-poll-id", "poll-id")
	if m.PollID != "" {
		unifiedpush = false
		cache = false
		email = ""
	}
	return cache, firebase, email, unifiedpush, nil
}

// handlePublishBody consumes the PUT/POST body and decides whether the body is an attachment or the message.
//
//  1. curl -X POST -H "Poll: 1234" ntfy.sh/...
//     If a message is flagged as poll request, the body does not matter and is discarded
//  2. curl -T somebinarydata.bin "ntfy.sh/mytopic?up=1"
//     If body is binary, encode as base64, if not do not encode
//  3. curl -H "Attach: http://example.com/file.jpg" ntfy.sh/mytopic
//     Body must be a message, because we attached an external URL
//  4. curl -T short.txt -H "Filename: short.txt" ntfy.sh/mytopic
//     Body must be attachment, because we passed a filename
//  5. curl -T file.txt ntfy.sh/mytopic
//     If file.txt is <= 4096 (message limit) and valid UTF-8, treat it as a message
//  6. curl -T file.txt ntfy.sh/mytopic
//     If file.txt is > message limit, treat it as an attachment
func (s *Server) handlePublishBody(r *http.Request, v *visitor, m *message, body *util.PeekedReadCloser, unifiedpush bool) error {
	if m.Event == pollRequestEvent { // Case 1
		return s.handleBodyDiscard(body)
	} else if unifiedpush {
		return s.handleBodyAsMessageAutoDetect(m, body) // Case 2
	} else if m.Attachment != nil && m.Attachment.URL != "" {
		return s.handleBodyAsTextMessage(m, body) // Case 3
	} else if m.Attachment != nil && m.Attachment.Name != "" {
		return s.handleBodyAsAttachment(r, v, m, body) // Case 4
	} else if !body.LimitReached && utf8.Valid(body.PeekedBytes) {
		return s.handleBodyAsTextMessage(m, body) // Case 5
	}
	return s.handleBodyAsAttachment(r, v, m, body) // Case 6
}

func (s *Server) handleBodyDiscard(body *util.PeekedReadCloser) error {
	_, err := io.Copy(io.Discard, body)
	_ = body.Close()
	return err
}

func (s *Server) handleBodyAsMessageAutoDetect(m *message, body *util.PeekedReadCloser) error {
	if utf8.Valid(body.PeekedBytes) {
		m.Message = string(body.PeekedBytes) // Do not trim
	} else {
		m.Message = base64.StdEncoding.EncodeToString(body.PeekedBytes)
		m.Encoding = encodingBase64
	}
	return nil
}

func (s *Server) handleBodyAsTextMessage(m *message, body *util.PeekedReadCloser) error {
	if !utf8.Valid(body.PeekedBytes) {
		return errHTTPBadRequestMessageNotUTF8
	}
	if len(body.PeekedBytes) > 0 { // Empty body should not override message (publish via GET!)
		m.Message = strings.TrimSpace(string(body.PeekedBytes)) // Truncates the message to the peek limit if required
	}
	if m.Attachment != nil && m.Attachment.Name != "" && m.Message == "" {
		m.Message = fmt.Sprintf(defaultAttachmentMessage, m.Attachment.Name)
	}
	return nil
}

func (s *Server) handleBodyAsAttachment(r *http.Request, v *visitor, m *message, body *util.PeekedReadCloser) error {
	if s.fileCache == nil || s.config.BaseURL == "" || s.config.AttachmentCacheDir == "" {
		return errHTTPBadRequestAttachmentsDisallowed
	}
	vinfo, err := v.Info()
	if err != nil {
		return err
	}
	attachmentExpiry := time.Now().Add(vinfo.Limits.AttachmentExpiryDuration).Unix()
	if m.Time > attachmentExpiry {
		return errHTTPBadRequestAttachmentsExpiryBeforeDelivery
	}
	contentLengthStr := r.Header.Get("Content-Length")
	if contentLengthStr != "" { // Early "do-not-trust" check, hard limit see below
		contentLength, err := strconv.ParseInt(contentLengthStr, 10, 64)
		if err == nil && (contentLength > vinfo.Stats.AttachmentTotalSizeRemaining || contentLength > vinfo.Limits.AttachmentFileSizeLimit) {
			return errHTTPEntityTooLargeAttachment
		}
	}
	if m.Attachment == nil {
		m.Attachment = &attachment{}
	}
	var ext string
	m.Attachment.Expires = attachmentExpiry
	m.Attachment.Type, ext = util.DetectContentType(body.PeekedBytes, m.Attachment.Name)
	m.Attachment.URL = fmt.Sprintf("%s/file/%s%s", s.config.BaseURL, m.ID, ext)
	if m.Attachment.Name == "" {
		m.Attachment.Name = fmt.Sprintf("attachment%s", ext)
	}
	if m.Message == "" {
		m.Message = fmt.Sprintf(defaultAttachmentMessage, m.Attachment.Name)
	}
	limiters := []util.Limiter{
		v.BandwidthLimiter(),
		util.NewFixedLimiter(vinfo.Limits.AttachmentFileSizeLimit),
		util.NewFixedLimiter(vinfo.Stats.AttachmentTotalSizeRemaining),
	}
	m.Attachment.Size, err = s.fileCache.Write(m.ID, body, limiters...)
	if err == util.ErrLimitReached {
		return errHTTPEntityTooLargeAttachment
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Server) handleSubscribeJSON(w http.ResponseWriter, r *http.Request, v *visitor) error {
	encoder := func(msg *message) (string, error) {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(&msg); err != nil {
			return "", err
		}
		return buf.String(), nil
	}
	return s.handleSubscribeHTTP(w, r, v, "application/x-ndjson", encoder)
}

func (s *Server) handleSubscribeSSE(w http.ResponseWriter, r *http.Request, v *visitor) error {
	encoder := func(msg *message) (string, error) {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(&msg); err != nil {
			return "", err
		}
		if msg.Event != messageEvent {
			return fmt.Sprintf("event: %s\ndata: %s\n", msg.Event, buf.String()), nil // Browser's .onmessage() does not fire on this!
		}
		return fmt.Sprintf("data: %s\n", buf.String()), nil
	}
	return s.handleSubscribeHTTP(w, r, v, "text/event-stream", encoder)
}

func (s *Server) handleSubscribeRaw(w http.ResponseWriter, r *http.Request, v *visitor) error {
	encoder := func(msg *message) (string, error) {
		if msg.Event == messageEvent { // only handle default events
			return strings.ReplaceAll(msg.Message, "\n", " ") + "\n", nil
		}
		return "\n", nil // "keepalive" and "open" events just send an empty line
	}
	return s.handleSubscribeHTTP(w, r, v, "text/plain", encoder)
}

func (s *Server) handleSubscribeHTTP(w http.ResponseWriter, r *http.Request, v *visitor, contentType string, encoder messageEncoder) error {
	logvr(v, r).Tag(tagSubscribe).Debug("HTTP stream connection opened")
	defer logvr(v, r).Tag(tagSubscribe).Debug("HTTP stream connection closed")
	if !v.SubscriptionAllowed() {
		return errHTTPTooManyRequestsLimitSubscriptions
	}
	defer v.RemoveSubscription()
	topics, topicsStr, err := s.topicsFromPath(r.URL.Path)
	if err != nil {
		return err
	}
	poll, since, scheduled, filters, err := parseSubscribeParams(r)
	if err != nil {
		return err
	}
	var wlock sync.Mutex
	defer func() {
		// Hack: This is the fix for a horrible data race that I have not been able to figure out in quite some time.
		// It appears to be happening when the Go HTTP code reads from the socket when closing the request (i.e. AFTER
		// this function returns), and causes a data race with the ResponseWriter. Locking wlock here silences the
		// data race detector. See https://github.com/binwiederhier/ntfy/issues/338#issuecomment-1163425889.
		wlock.TryLock()
	}()
	sub := func(v *visitor, msg *message) error {
		if !filters.Pass(msg) {
			return nil
		}
		m, err := encoder(msg)
		if err != nil {
			return err
		}
		wlock.Lock()
		defer wlock.Unlock()
		if _, err := w.Write([]byte(m)); err != nil {
			return err
		}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		return nil
	}
	w.Header().Set("Access-Control-Allow-Origin", s.config.AccessControlAllowOrigin) // CORS, allow cross-origin requests
	w.Header().Set("Content-Type", contentType+"; charset=utf-8")                    // Android/Volley client needs charset!
	if poll {
		return s.sendOldMessages(topics, since, scheduled, v, sub)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscriberIDs := make([]int, 0)
	for _, t := range topics {
		subscriberIDs = append(subscriberIDs, t.Subscribe(sub, v.MaybeUserID(), cancel))
	}
	defer func() {
		for i, subscriberID := range subscriberIDs {
			topics[i].Unsubscribe(subscriberID) // Order!
		}
	}()
	if err := sub(v, newOpenMessage(topicsStr)); err != nil { // Send out open message
		return err
	}
	if err := s.sendOldMessages(topics, since, scheduled, v, sub); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.Context().Done():
			return nil
		case <-time.After(s.config.KeepaliveInterval):
			logvr(v, r).Tag(tagSubscribe).Trace("Sending keepalive message")
			v.Keepalive()
			if err := sub(v, newKeepaliveMessage(topicsStr)); err != nil { // Send keepalive message
				return err
			}
		}
	}
}

func (s *Server) handleSubscribeWS(w http.ResponseWriter, r *http.Request, v *visitor) error {
	if strings.ToLower(r.Header.Get("Upgrade")) != "websocket" {
		return errHTTPBadRequestWebSocketsUpgradeHeaderMissing
	}
	if !v.SubscriptionAllowed() {
		return errHTTPTooManyRequestsLimitSubscriptions
	}
	defer v.RemoveSubscription()
	logvr(v, r).Tag(tagWebsocket).Debug("WebSocket connection opened")
	defer logvr(v, r).Tag(tagWebsocket).Debug("WebSocket connection closed")
	topics, topicsStr, err := s.topicsFromPath(r.URL.Path)
	if err != nil {
		return err
	}
	poll, since, scheduled, filters, err := parseSubscribeParams(r)
	if err != nil {
		return err
	}
	upgrader := &websocket.Upgrader{
		ReadBufferSize:  wsBufferSize,
		WriteBufferSize: wsBufferSize,
		CheckOrigin: func(r *http.Request) bool {
			return true // We're open for business!
		},
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Subscription connections can be canceled externally, see topic.CancelSubscribers
	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use errgroup to run WebSocket reader and writer in Go routines
	var wlock sync.Mutex
	g, gctx := errgroup.WithContext(cancelCtx)
	g.Go(func() error {
		pongWait := s.config.KeepaliveInterval + wsPongWait
		conn.SetReadLimit(wsReadLimit)
		if err := conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
			return err
		}
		conn.SetPongHandler(func(appData string) error {
			logvr(v, r).Tag(tagWebsocket).Trace("Received WebSocket pong")
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})
		for {
			_, _, err := conn.NextReader()
			if err != nil {
				return err
			}
			select {
			case <-gctx.Done():
				return nil
			default:
			}
		}
	})
	g.Go(func() error {
		ping := func() error {
			wlock.Lock()
			defer wlock.Unlock()
			if err := conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return err
			}
			logvr(v, r).Tag(tagWebsocket).Trace("Sending WebSocket ping")
			return conn.WriteMessage(websocket.PingMessage, nil)
		}
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-cancelCtx.Done():
				logvr(v, r).Tag(tagWebsocket).Trace("Cancel received, closing subscriber connection")
				conn.Close()
				return &websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "subscription was canceled"}
			case <-time.After(s.config.KeepaliveInterval):
				v.Keepalive()
				if err := ping(); err != nil {
					return err
				}
			}
		}
	})
	sub := func(v *visitor, msg *message) error {
		if !filters.Pass(msg) {
			return nil
		}
		wlock.Lock()
		defer wlock.Unlock()
		if err := conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
			return err
		}
		return conn.WriteJSON(msg)
	}
	w.Header().Set("Access-Control-Allow-Origin", s.config.AccessControlAllowOrigin) // CORS, allow cross-origin requests
	if poll {
		return s.sendOldMessages(topics, since, scheduled, v, sub)
	}
	subscriberIDs := make([]int, 0)
	for _, t := range topics {
		subscriberIDs = append(subscriberIDs, t.Subscribe(sub, v.MaybeUserID(), cancel))
	}
	defer func() {
		for i, subscriberID := range subscriberIDs {
			topics[i].Unsubscribe(subscriberID) // Order!
		}
	}()
	if err := sub(v, newOpenMessage(topicsStr)); err != nil { // Send out open message
		return err
	}
	if err := s.sendOldMessages(topics, since, scheduled, v, sub); err != nil {
		return err
	}
	err = g.Wait()
	if err != nil && websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNoStatusReceived) {
		logvr(v, r).Tag(tagWebsocket).Err(err).Fields(websocketErrorContext(err)).Trace("WebSocket connection closed")
		return nil // Normal closures are not errors; note: "1006 (abnormal closure)" is treated as normal, because people disconnect a lot
	}
	return err
}

func parseSubscribeParams(r *http.Request) (poll bool, since sinceMarker, scheduled bool, filters *queryFilter, err error) {
	poll = readBoolParam(r, false, "x-poll", "poll", "po")
	scheduled = readBoolParam(r, false, "x-scheduled", "scheduled", "sched")
	since, err = parseSince(r, poll)
	if err != nil {
		return
	}
	filters, err = parseQueryFilters(r)
	if err != nil {
		return
	}
	return
}

// sendOldMessages selects old messages from the messageCache and calls sub for each of them. It uses since as the
// marker, returning only messages that are newer than the marker.
func (s *Server) sendOldMessages(topics []*topic, since sinceMarker, scheduled bool, v *visitor, sub subscriber) error {
	if since.IsNone() {
		return nil
	}
	messages := make([]*message, 0)
	for _, t := range topics {
		topicMessages, err := s.messageCache.Messages(t.ID, since, scheduled)
		if err != nil {
			return err
		}
		messages = append(messages, topicMessages...)
	}
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Time < messages[j].Time
	})
	for _, m := range messages {
		if err := sub(v, m); err != nil {
			return err
		}
	}
	return nil
}

// parseSince returns a timestamp identifying the time span from which cached messages should be received.
//
// Values in the "since=..." parameter can be either a unix timestamp or a duration (e.g. 12h), or
// "all" for all messages.
func parseSince(r *http.Request, poll bool) (sinceMarker, error) {
	since := readParam(r, "x-since", "since", "si")

	// Easy cases (empty, all, none)
	if since == "" {
		if poll {
			return sinceAllMessages, nil
		}
		return sinceNoMessages, nil
	} else if since == "all" {
		return sinceAllMessages, nil
	} else if since == "none" {
		return sinceNoMessages, nil
	}

	// ID, timestamp, duration
	if validMessageID(since) {
		return newSinceID(since), nil
	} else if s, err := strconv.ParseInt(since, 10, 64); err == nil {
		return newSinceTime(s), nil
	} else if d, err := time.ParseDuration(since); err == nil {
		return newSinceTime(time.Now().Add(-1 * d).Unix()), nil
	}
	return sinceNoMessages, errHTTPBadRequestSinceInvalid
}

func (s *Server) handleOptions(w http.ResponseWriter, _ *http.Request, _ *visitor) error {
	w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, PATCH, DELETE")
	w.Header().Set("Access-Control-Allow-Origin", s.config.AccessControlAllowOrigin) // CORS, allow cross-origin requests
	w.Header().Set("Access-Control-Allow-Headers", "*")                              // CORS, allow auth via JS // FIXME is this terrible?
	return nil
}

func (s *Server) topicFromPath(path string) (*topic, error) {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return nil, errHTTPBadRequestTopicInvalid
	}
	return s.topicFromID(parts[1])
}

func (s *Server) topicsFromPath(path string) ([]*topic, string, error) {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return nil, "", errHTTPBadRequestTopicInvalid
	}
	topicIDs := util.SplitNoEmpty(parts[1], ",")
	topics, err := s.topicsFromIDs(topicIDs...)
	if err != nil {
		return nil, "", errHTTPBadRequestTopicInvalid
	}
	return topics, parts[1], nil
}

func (s *Server) topicsFromIDs(ids ...string) ([]*topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	topics := make([]*topic, 0)
	for _, id := range ids {
		if util.Contains(s.config.DisallowedTopics, id) {
			return nil, errHTTPBadRequestTopicDisallowed
		}
		if _, ok := s.topics[id]; !ok {
			if len(s.topics) >= s.config.TotalTopicLimit {
				return nil, errHTTPTooManyRequestsLimitTotalTopics
			}
			s.topics[id] = newTopic(id)
		}
		topics = append(topics, s.topics[id])
	}
	return topics, nil
}

func (s *Server) topicFromID(id string) (*topic, error) {
	topics, err := s.topicsFromIDs(id)
	if err != nil {
		return nil, err
	}
	return topics[0], nil
}

func (s *Server) runSMTPServer() error {
	s.smtpServerBackend = newMailBackend(s.config, s.handle)
	s.smtpServer = smtp.NewServer(s.smtpServerBackend)
	s.smtpServer.Addr = s.config.SMTPServerListen
	s.smtpServer.Domain = s.config.SMTPServerDomain
	s.smtpServer.ReadTimeout = 10 * time.Second
	s.smtpServer.WriteTimeout = 10 * time.Second
	s.smtpServer.MaxMessageBytes = 1024 * 1024 // Must be much larger than message size (headers, multipart, etc.)
	s.smtpServer.MaxRecipients = 1
	s.smtpServer.AllowInsecureAuth = true
	return s.smtpServer.ListenAndServe()
}

func (s *Server) runManager() {
	for {
		select {
		case <-time.After(s.config.ManagerInterval):
			log.
				Tag(tagManager).
				Timing(s.execManager).
				Debug("Manager finished")
		case <-s.closeChan:
			return
		}
	}
}

// runStatsResetter runs once a day (usually midnight UTC) to reset all the visitor's message and
// email counters. The stats are used to display the counters in the web app, as well as for rate limiting.
func (s *Server) runStatsResetter() {
	for {
		runAt := util.NextOccurrenceUTC(s.config.VisitorStatsResetTime, time.Now())
		timer := time.NewTimer(time.Until(runAt))
		log.Tag(tagResetter).Debug("Waiting until %v to reset visitor stats", runAt)
		select {
		case <-timer.C:
			log.Tag(tagResetter).Debug("Running stats resetter")
			s.resetStats()
		case <-s.closeChan:
			log.Tag(tagResetter).Debug("Stopping stats resetter")
			timer.Stop()
			return
		}
	}
}

func (s *Server) resetStats() {
	log.Info("Resetting all visitor stats (daily task)")
	s.mu.Lock()
	defer s.mu.Unlock() // Includes the database query to avoid races with other processes
	for _, v := range s.visitors {
		v.ResetStats()
	}
	if s.userManager != nil {
		if err := s.userManager.ResetStats(); err != nil {
			log.Tag(tagResetter).Warn("Failed to write to database: %s", err.Error())
		}
	}
}

func (s *Server) runFirebaseKeepaliver() {
	if s.firebaseClient == nil {
		return
	}
	v := newVisitor(s.config, s.messageCache, s.userManager, netip.IPv4Unspecified(), nil) // Background process, not a real visitor, uses IP 0.0.0.0
	for {
		select {
		case <-time.After(s.config.FirebaseKeepaliveInterval):
			s.sendToFirebase(v, newKeepaliveMessage(firebaseControlTopic))
		case <-time.After(s.config.FirebasePollInterval):
			s.sendToFirebase(v, newKeepaliveMessage(firebasePollTopic))
		case <-s.closeChan:
			return
		}
	}
}

func (s *Server) runDelayedSender() {
	for {
		select {
		case <-time.After(s.config.DelayedSenderInterval):
			if err := s.sendDelayedMessages(); err != nil {
				log.Tag(tagPublish).Err(err).Warn("Error sending delayed messages")
			}
		case <-s.closeChan:
			return
		}
	}
}

func (s *Server) sendDelayedMessages() error {
	messages, err := s.messageCache.MessagesDue()
	if err != nil {
		return err
	}
	for _, m := range messages {
		var u *user.User
		if s.userManager != nil && m.User != "" {
			u, err = s.userManager.User(m.User)
			if err != nil {
				log.With(m).Err(err).Warn("Error sending delayed message")
				continue
			}
		}
		v := s.visitor(m.Sender, u)
		if err := s.sendDelayedMessage(v, m); err != nil {
			logvm(v, m).Err(err).Warn("Error sending delayed message")
		}
	}
	return nil
}

func (s *Server) sendDelayedMessage(v *visitor, m *message) error {
	logvm(v, m).Debug("Sending delayed message")
	s.mu.Lock()
	t, ok := s.topics[m.Topic] // If no subscribers, just mark message as published
	s.mu.Unlock()
	if ok {
		go func() {
			// We do not rate-limit messages here, since we've rate limited them in the PUT/POST handler
			if err := t.Publish(v, m); err != nil {
				logvm(v, m).Err(err).Warn("Unable to publish message")
			}
		}()
	}
	if s.firebaseClient != nil { // Firebase subscribers may not show up in topics map
		go s.sendToFirebase(v, m)
	}
	if s.config.UpstreamBaseURL != "" {
		go s.forwardPollRequest(v, m)
	}
	if err := s.messageCache.MarkPublished(m); err != nil {
		return err
	}
	return nil
}

// transformBodyJSON peeks the request body, reads the JSON, and converts it to headers
// before passing it on to the next handler. This is meant to be used in combination with handlePublish.
func (s *Server) transformBodyJSON(next handleFunc) handleFunc {
	return func(w http.ResponseWriter, r *http.Request, v *visitor) error {
		m, err := readJSONWithLimit[publishMessage](r.Body, s.config.MessageLimit*2, false) // 2x to account for JSON format overhead
		if err != nil {
			return err
		}
		if !topicRegex.MatchString(m.Topic) {
			return errHTTPBadRequestTopicInvalid
		}
		if m.Message == "" {
			m.Message = emptyMessageBody
		}
		r.URL.Path = "/" + m.Topic
		r.Body = io.NopCloser(strings.NewReader(m.Message))
		if m.Title != "" {
			r.Header.Set("X-Title", m.Title)
		}
		if m.Priority != 0 {
			r.Header.Set("X-Priority", fmt.Sprintf("%d", m.Priority))
		}
		if m.Tags != nil && len(m.Tags) > 0 {
			r.Header.Set("X-Tags", strings.Join(m.Tags, ","))
		}
		if m.Attach != "" {
			r.Header.Set("X-Attach", m.Attach)
		}
		if m.Filename != "" {
			r.Header.Set("X-Filename", m.Filename)
		}
		if m.Click != "" {
			r.Header.Set("X-Click", m.Click)
		}
		if m.Icon != "" {
			r.Header.Set("X-Icon", m.Icon)
		}
		if len(m.Actions) > 0 {
			actionsStr, err := json.Marshal(m.Actions)
			if err != nil {
				return errHTTPBadRequestMessageJSONInvalid
			}
			r.Header.Set("X-Actions", string(actionsStr))
		}
		if m.Email != "" {
			r.Header.Set("X-Email", m.Email)
		}
		if m.Delay != "" {
			r.Header.Set("X-Delay", m.Delay)
		}
		return next(w, r, v)
	}
}

func (s *Server) transformMatrixJSON(next handleFunc) handleFunc {
	return func(w http.ResponseWriter, r *http.Request, v *visitor) error {
		newRequest, err := newRequestFromMatrixJSON(r, s.config.BaseURL, s.config.MessageLimit)
		if err != nil {
			logvr(v, r).Tag(tagMatrix).Err(err).Trace("Invalid Matrix request")
			return err
		}
		if err := next(w, newRequest, v); err != nil {
			return &errMatrix{pushKey: newRequest.Header.Get(matrixPushKeyHeader), err: err}
		}
		return nil
	}
}

func (s *Server) authorizeTopicWrite(next handleFunc) handleFunc {
	return s.autorizeTopic(next, user.PermissionWrite)
}

func (s *Server) authorizeTopicRead(next handleFunc) handleFunc {
	return s.autorizeTopic(next, user.PermissionRead)
}

func (s *Server) autorizeTopic(next handleFunc, perm user.Permission) handleFunc {
	return func(w http.ResponseWriter, r *http.Request, v *visitor) error {
		if s.userManager == nil {
			return next(w, r, v)
		}
		topics, _, err := s.topicsFromPath(r.URL.Path)
		if err != nil {
			return err
		}
		u := v.User()
		for _, t := range topics {
			if err := s.userManager.Authorize(u, t.ID, perm); err != nil {
				logvr(v, r).Err(err).Field("message_topic", t.ID).Debug("Access to topic %s not authorized", t.ID)
				return errHTTPForbidden
			}
		}
		return next(w, r, v)
	}
}

// maybeAuthenticate reads the "Authorization" header and will try to authenticate the user
// if it is set.
//
//   - If the header is not set, an IP-based visitor is returned
//   - If the header is set, authenticate will be called to check the username/password (Basic auth),
//     or the token (Bearer auth), and read the user from the database
//
// This function will ALWAYS return a visitor, even if an error occurs (e.g. unauthorized), so
// that subsequent logging calls still have a visitor context.
func (s *Server) maybeAuthenticate(r *http.Request) (*visitor, error) {
	// Read "Authorization" header value, and exit out early if it's not set
	ip := extractIPAddress(r, s.config.BehindProxy)
	vip := s.visitor(ip, nil)
	header, err := readAuthHeader(r)
	if err != nil {
		return vip, err
	} else if header == "" {
		return vip, nil
	} else if s.userManager == nil {
		return vip, errHTTPUnauthorized
	}
	// If we're trying to auth, check the rate limiter first
	if !vip.AuthAllowed() {
		return vip, errHTTPTooManyRequestsLimitAuthFailure // Always return visitor, even when error occurs!
	}
	u, err := s.authenticate(r, header)
	if err != nil {
		vip.AuthFailed()
		logr(r).Err(err).Debug("Authentication failed")
		return vip, errHTTPUnauthorized // Always return visitor, even when error occurs!
	}
	// Authentication with user was successful
	return s.visitor(ip, u), nil
}

// authenticate a user based on basic auth username/password (Authorization: Basic ...), or token auth (Authorization: Bearer ...).
// The Authorization header can be passed as a header or the ?auth=... query param. The latter is required only to
// support the WebSocket JavaScript class, which does not support passing headers during the initial request. The auth
// query param is effectively doubly base64 encoded. Its format is base64(Basic base64(user:pass)).
func (s *Server) authenticate(r *http.Request, header string) (user *user.User, err error) {
	if strings.HasPrefix(header, "Bearer") {
		return s.authenticateBearerAuth(r, strings.TrimSpace(strings.TrimPrefix(header, "Bearer")))
	}
	return s.authenticateBasicAuth(r, header)
}

// readAuthHeader reads the raw value of the Authorization header, either from the actual HTTP header,
// or from the ?auth... query parameter
func readAuthHeader(r *http.Request) (string, error) {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	queryParam := readQueryParam(r, "authorization", "auth")
	if queryParam != "" {
		a, err := base64.RawURLEncoding.DecodeString(queryParam)
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(string(a))
	}
	return value, nil
}

func (s *Server) authenticateBasicAuth(r *http.Request, value string) (user *user.User, err error) {
	r.Header.Set("Authorization", value)
	username, password, ok := r.BasicAuth()
	if !ok {
		return nil, errors.New("invalid basic auth")
	} else if username == "" {
		return s.authenticateBearerAuth(r, password) // Treat password as token
	}
	return s.userManager.Authenticate(username, password)
}

func (s *Server) authenticateBearerAuth(r *http.Request, token string) (*user.User, error) {
	u, err := s.userManager.AuthenticateToken(token)
	if err != nil {
		return nil, err
	}
	ip := extractIPAddress(r, s.config.BehindProxy)
	go s.userManager.EnqueueTokenUpdate(token, &user.TokenUpdate{
		LastAccess: time.Now(),
		LastOrigin: ip,
	})
	return u, nil
}

func (s *Server) visitor(ip netip.Addr, user *user.User) *visitor {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := visitorID(ip, user)
	v, exists := s.visitors[id]
	if !exists {
		s.visitors[id] = newVisitor(s.config, s.messageCache, s.userManager, ip, user)
		return s.visitors[id]
	}
	v.Keepalive()
	v.SetUser(user) // Always update with the latest user, may be nil!
	return v
}

func (s *Server) writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", s.config.AccessControlAllowOrigin) // CORS, allow cross-origin requests
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return err
	}
	return nil
}
