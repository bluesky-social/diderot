// Package config parses and validates diderot's environment-driven
// configuration.
//
// Keep this file narrow. A field lands here when a runtime
// component (serve, an ingest worker, a query handler) actually
// reads it. Pre-declared options for features that don't exist yet
// rot. When a new field arrives, add it with the component it
// serves in the same commit.
package config

import (
	"crypto/x509"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DIDEROT_DEPLOYMENT and DIDEROT_LEADER_BACKEND values.
const (
	DeploymentKubernetes    = "kubernetes"
	DeploymentStandalone    = "standalone"
	LeaderBackendKubernetes = "kubernetes"
	LeaderBackendPostgres   = "postgres"
)

type Config struct {
	// DBURL is the Postgres connection string. The diderot role
	// holds DDL on its schema (CREATE + ALTER), which the binary
	// uses to run migrations in-process on startup via
	// internal/migrations. Runtime traffic uses the same URL.
	DBURL string
	// DBIAMAuth logs in with RDS IAM tokens (DIDEROT_DB_IAM_AUTH=true);
	// DBURL then names the user and carries no password. See
	// internal/dbconn.
	DBIAMAuth bool

	// AdminGroups is the group allowlist for the administrator web UI, and
	// for Ask before the install is live. The ordinary ask/retrieve/list
	// surfaces otherwise do not use it. Browser identities carry the trusted
	// proxy's groups header; API identities carry their authenticator's
	// groups (the OIDC groups claim, or a static token's configured groups).
	AdminGroups []string
	// SelfServiceGroups may use the owner-bound agent-session inventory and
	// privacy toggle. It is intentionally separate from administrator access.
	SelfServiceGroups       []string
	AuthorizationPolicyPath string
	// PublicURL is the browser-reachable Diderot origin used in operator
	// notifications. It must be an http(s) origin without a path, and is
	// required: the browser UI checks mutation origins against it.
	PublicURL string

	// Deployment declares where serve runs (DIDEROT_DEPLOYMENT):
	// DeploymentKubernetes (default) refuses to serve outside a pod;
	// DeploymentStandalone is a deliberate deployment anywhere else.
	Deployment string

	// LeaderElection toggles leader election (DIDEROT_LEADER_ELECTION);
	// disable it in local dev to run leader-only work inline.
	// LeaderBackend picks the mechanism (DIDEROT_LEADER_BACKEND):
	// a Kubernetes Lease named LeaseName in Namespace (POD_NAMESPACE; empty
	// is the pod's own namespace), or a Postgres advisory lock keyed on
	// LeaseName. It defaults to kubernetes, or to postgres for a standalone
	// deployment.
	Namespace      string
	LeaseName      string
	LeaderElection bool
	LeaderBackend  string

	// Qdrant is the vector store. Addr is "host:port" (gRPC) and required;
	// the collection is the embedding model's, chosen on the Models page.
	// QdrantTLS dials it over TLS, trusting the CAs of
	// DIDEROT_QDRANT_CA_FILE (QdrantRootCAs), or the system's CAs when
	// QdrantRootCAs is nil.
	QdrantAddr              string
	QdrantAPIKey            string
	QdrantTLS               bool
	QdrantRootCAs           *x509.CertPool
	QdrantShardNumber       uint32
	QdrantReplicationFactor uint32

	// GatewayAPIKey is the bearer /mcp requires from the gateway in front
	// of it.
	GatewayAPIKey string

	// EmbedWorkers and EmbedBatch size the embedder, whose model is chosen
	// on the Models page.
	EmbedWorkers int
	EmbedBatch   int

	// ConfigKeys are 32-byte AES-256-GCM keys that encrypt connector
	// secrets pasted on their pages; the first encrypts and all decrypt,
	// for rotation. Empty refuses pasted secrets.
	ConfigKeys [][]byte

	// AuthMode selects how /v1 bearer callers are authenticated:
	// AuthModeOIDC (default) verifies IdP-issued JWTs, AuthModeStatic looks
	// tokens up by SHA-256 in StaticTokensFile, and AuthModeNone treats every
	// request as NoneEmail with the administrator and self-service groups. None
	// refuses to start unless every listener binds a literal loopback IP.
	AuthMode         string
	StaticTokensFile string
	NoneEmail        string

	// OIDC operator auth. In oidc mode every /v1 route requires a verified
	// access token: signature, issuer, audience, expiry. WHO can obtain such a
	// token is governed by the IdP application's access policy; diderot only
	// checks that the token is genuine.
	//
	// OIDCIssuer is the `iss` to require on tokens. OIDCJWKSURL is where the
	// server fetches signing keys; it may be a different host than the issuer
	// (an in-cluster IdP service), and empty means OIDC discovery from the
	// issuer. OIDCAudience is the access-token `aud`.
	//
	// oidc mode requires OIDCIssuer and OIDCAudience.
	OIDCIssuer   string
	OIDCJWKSURL  string
	OIDCAudience string
	// OIDCScopes are the scopes that make the IdP put the identity claims in
	// its tokens: the browser sign-in requests them, and in oidc mode /mcp
	// advertises them to MCP clients. They include openid.
	OIDCScopes []string
	// OIDCGroupsClaim and OIDCEmailClaim are claim paths: a literal claim
	// name first, else dot-separated object keys (Keycloak's
	// "realm_access.roles"). Namespaced URL claims work as literal names.
	OIDCGroupsClaim string
	OIDCEmailClaim  string
	// OIDCRequireEmailVerified (default true) drops the token's email unless
	// it carries email_verified=true. Email grants Slack channel scopes,
	// session ownership, and self-service, so an unverified address must not
	// reach them unless the operator vouches for the IdP by disabling this.
	OIDCRequireEmailVerified bool
	// OIDCAllowedEmailDomains (lowercased; empty admits any) refuses OIDC
	// bearers and browser sign-ins whose accepted email is missing or at
	// another domain, for an IdP that signs in accounts beyond the
	// organization's (Google). OIDCHostedDomainClaim (Google: "hd")
	// additionally requires that claim to name an allowed domain.
	OIDCAllowedEmailDomains []string
	OIDCHostedDomainClaim   string

	// TrustedProxyCIDRs lists the authenticating reverse proxies whose
	// identity headers the browser UI and /mcp honor. The request's immediate
	// peer (RemoteAddr) must fall inside one; X-Forwarded-For is never read.
	// Empty = identity headers are ignored entirely. The header names have no
	// defaults: a deployment might set X-Auth-Request-Email,
	// X-Auth-Request-Groups (with its delimiter), and X-Auth-Request-User for
	// /mcp.
	TrustedProxyCIDRs           []netip.Prefix
	TrustedProxyEmailHeader     string
	TrustedProxyGroupsHeader    string
	TrustedProxyGroupsDelimiter string
	// TrustedProxyMCPEmailHeader is the caller-email header on /mcp, which
	// arrives through a gateway rather than the browser proxy. Empty
	// means TrustedProxyEmailHeader.
	TrustedProxyMCPEmailHeader string

	// BrowserAuth selects the browser UI's external identity source (/admin,
	// /sessions): empty or BrowserAuthTrustedProxy reads trusted-proxy
	// headers, and BrowserAuthOIDC is the built-in sign-in, which never reads
	// them (the trusted-proxy settings then serve /mcp alone). With
	// BrowserSessionKeys, local sign-in (the bootstrap admin, and static
	// tokens in static mode) works beside either. None mode leaves it empty:
	// its fixed principal needs no sign-in.
	BrowserAuth string
	// BrowserOIDCClientID and BrowserOIDCClientSecret (empty for a public
	// client) are the IdP's web client for DIDEROT_OIDC_ISSUER, registered
	// with redirect URI PublicURL + /auth/callback.
	BrowserOIDCClientID     string
	BrowserOIDCClientSecret string
	// BrowserSessionKeys are 32-byte AES-256-GCM keys; the first seals and
	// all open, for rotation. They seal the session cookie of every browser
	// sign-in method; empty leaves the browser with trusted-proxy identity
	// only. Required with BrowserAuthOIDC; refused in none mode.
	BrowserSessionKeys [][]byte
	// BrowserSessionTTL is the absolute session lifetime and
	// BrowserSessionIdleTimeout ends a session left unused.
	BrowserSessionTTL         time.Duration
	BrowserSessionIdleTimeout time.Duration
	// BrowserSessionInsecureCookie drops Secure for a plain-HTTP loopback
	// PublicURL during development.
	BrowserSessionInsecureCookie bool

	// Ask configures the agentic Q&A subsystem (POST /v1/ask, the
	// Slack @diderot path). Disabled by default while the feature
	// stabilizes; flip DIDEROT_ASK_ENABLED=true on the deploy that's
	// ready to expose it.
	//
	// Ask inference goes to the model's provider using the provider-native
	// protocol selected by the configured model profile. The model and every
	// limit on what Ask does for one question are chosen on the Models page
	// (the models row of internal/settings).
	AskEnabled bool
	// AskReadinessTimeout bounds each model readiness probe; the
	// default is 5s so a hung gateway can't stall the probe loop.
	AskReadinessTimeout time.Duration
	// AskReadinessInterval is how often the readiness probe re-checks the
	// gateway. The first check always runs at startup; the probe is a
	// one-token inference, so 30s of checks is negligible cost.
	AskReadinessInterval time.Duration

	// ImagesBucket is the S3 bucket the caption worker mirrors
	// image attachments into. Source-agnostic: slack file uploads,
	// agent_session image blocks, and future sources all land
	// here. Empty disables the caption worker AND the file-proxy
	// backend — without somewhere to write bytes, neither side
	// has anything to do. Layout is content-addressed by sha256:
	// `<source>/<sha256[0:2]>/<sha256>.<ext>`. It also disables document
	// extraction and Ask panel images.
	ImagesBucket string
	// ImagesRegion is the S3 signing region; empty defers to the AWS SDK's
	// resolution (AWS_REGION, the shared config profile).
	ImagesRegion string
	// ImagesEndpoint and ImagesPathStyle point the mirror at an
	// S3-compatible store (MinIO, R2) instead of AWS.
	ImagesEndpoint  string
	ImagesPathStyle bool
	// CaptionWorkers is the per-pod caption-worker concurrency.
	// Zero (env unset / not parseable) means "use the package
	// default", which files.Config.fillDefaults resolves to 4.
	// Raising it drains a historical backfill faster; live ingest
	// rarely needs more than one caption request at a time, so
	// concurrency mostly matters for backfill.
	CaptionWorkers int

	// Document ingest (internal/attachments/docs). Extracts text
	// from document attachments (PDF, docx, xlsx) via the model the Models
	// page chooses for document extraction. csv/txt/html are out of scope on
	// principle, not size; see the docs package. The Slack and Zendesk
	// connectors' Index documents settings each turn an extraction worker on;
	// each Slack channel opts in separately.
	//
	// DocExtractWorkers is the extraction concurrency. Zero means the
	// in-code default (2), deliberately below the caption worker's:
	// each extraction is tens of thousands of input tokens and a model call
	// that can run for tens of seconds (docs.perCallTimeout bounds a
	// stalled call at 20m), on capacity shared with the user-facing ask
	// loop.
	DocExtractWorkers int

	// Session ingest (internal/sessioningest). Leader-only worker that
	// reads one source: the session feed while that connector is enabled,
	// or pushed sessions (SessionPush). It stores a Diderot-owned durable
	// cursor, filters eligibility with a validated owner/harness/source
	// policy, and indexes selected redacted Bsky Session v1 revisions
	// canonically (replacement + tombstones).
	//
	// SessionIngestTick is the poll cadence. Plan says ~30s; we let
	// the operator raise it. Zero → default (30s).
	SessionIngestTick time.Duration
	// SessionIngestPageSize is the max change rows fetched per page
	// drain. Zero → default (100).
	SessionIngestPageSize int
	// SessionIngestBatchSize caps the sessions the index pass
	// processes per tick. Zero, or anything above 500, means 100.
	SessionIngestBatchSize int
	// SessionIngestConcurrency bounds how many sessions the index pass
	// publishes in parallel per tick. Zero → default (1).
	SessionIngestConcurrency      int
	SessionIngestChunkConcurrency int
	// SessionIngestChunkBatchSize caps the native map-summary chunk
	// events the level-triggered chunk reconciler upserts + tombstones
	// per tick. Zero → default (100).
	SessionIngestChunkBatchSize int
	// SessionIngestCodexApp is populated by DIDEROT_SESSION_INGEST_CODEX_APP
	// and rides into the policy default's app include list.
	SessionIngestCodexApp string
	// SessionIngestExcludeApp ships the in-code policy's default
	// excluded app (DIDEROT_SESSION_INGEST_EXCLUDE_APP). Kept
	// separate from the full policy-filter envs below.
	SessionIngestExcludeApp string
	// SessionIngestPolicyPath, when set, points at a mount-able YAML
	// policy file containing the sessionIngest include /
	// exclude block. Its effective content + hash drive
	// policy reconciliation (a hash change resets the cursor). Empty =
	// in-code default policy.
	SessionIngestPolicyPath string
	// SessionIngestMaxRevisionBytes caps the post-decompression size
	// of a fetched Bsky Session v1 revision before it is rejected.
	// It is mandatory and non-zero whenever session ingest is enabled.
	SessionIngestMaxRevisionBytes int64
	// SessionPush configures push ingest (DIDEROT_SESSION_INGEST_SOURCE=push).
	SessionPush SessionPushConfig
}

// removedEnv are the deployment variables a release removed, each with where
// its setting went: an admin page, the variable that replaced it, or "" when
// nothing did. Load refuses to run while any is set, so a half-done move
// fails loudly instead of quietly running without the setting. A release
// that removes a variable adds it here and to the Removed table of
// docs/configuration.md. No release has removed one yet.
var removedEnv []struct{ page, name string }

func checkRemovedEnv() error {
	var moved, dropped, pages []string
	for _, removed := range removedEnv {
		if os.Getenv(removed.name) == "" {
			continue
		}
		if removed.page == "" {
			dropped = append(dropped, removed.name)
			continue
		}
		moved = append(moved, removed.name)
		if !slices.Contains(pages, removed.page) {
			pages = append(pages, removed.page)
		}
	}
	var problems []string
	if len(moved) > 0 {
		problems = append(problems, fmt.Sprintf("%s: removed; set them at %s instead", strings.Join(moved, ", "), strings.Join(pages, " and ")))
	}
	if len(dropped) > 0 {
		problems = append(problems, fmt.Sprintf("%s: removed; nothing replaces them", strings.Join(dropped, ", ")))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s (docs/configuration.md#removed). Unset them to start", strings.Join(problems, "; "))
	}
	return nil
}

func Load() (*Config, error) {
	if err := checkRemovedEnv(); err != nil {
		return nil, err
	}
	cfg := &Config{
		Namespace: strings.TrimSpace(os.Getenv("POD_NAMESPACE")),
		LeaseName: "diderot-leader",
	}

	cfg.DBURL = os.Getenv("DIDEROT_DB_URL")
	if cfg.DBURL == "" {
		return nil, fmt.Errorf("DIDEROT_DB_URL is required")
	}
	if raw := os.Getenv("DIDEROT_DB_IAM_AUTH"); raw != "" {
		iamAuth, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("DIDEROT_DB_IAM_AUTH: %w", err)
		}
		cfg.DBIAMAuth = iamAuth
	}
	cfg.AuthorizationPolicyPath = os.Getenv("DIDEROT_AUTHORIZATION_POLICY_PATH")

	var err error
	if cfg.AdminGroups, err = requiredGroups("DIDEROT_ADMIN_GROUPS", "the groups whose members administer Diderot"); err != nil {
		return nil, err
	}
	if cfg.SelfServiceGroups, err = requiredGroups("DIDEROT_SELF_SERVICE_GROUPS", "the groups whose members may manage their own agent sessions"); err != nil {
		return nil, err
	}

	cfg.Deployment = getenvDefault("DIDEROT_DEPLOYMENT", DeploymentKubernetes)
	if cfg.Deployment != DeploymentKubernetes && cfg.Deployment != DeploymentStandalone {
		return nil, fmt.Errorf("DIDEROT_DEPLOYMENT must be %q or %q", DeploymentKubernetes, DeploymentStandalone)
	}
	cfg.LeaderElection = getenvDefault("DIDEROT_LEADER_ELECTION", "true") != "false"
	defaultLeaderBackend := LeaderBackendKubernetes
	if cfg.Deployment == DeploymentStandalone {
		defaultLeaderBackend = LeaderBackendPostgres
	}
	cfg.LeaderBackend = getenvDefault("DIDEROT_LEADER_BACKEND", defaultLeaderBackend)
	switch cfg.LeaderBackend {
	case LeaderBackendPostgres:
	case LeaderBackendKubernetes:
		if cfg.Deployment == DeploymentStandalone {
			return nil, fmt.Errorf("DIDEROT_LEADER_BACKEND=%s needs DIDEROT_DEPLOYMENT=%s; a standalone deployment elects through %s",
				LeaderBackendKubernetes, DeploymentKubernetes, LeaderBackendPostgres)
		}
	default:
		return nil, fmt.Errorf("DIDEROT_LEADER_BACKEND must be %q or %q", LeaderBackendKubernetes, LeaderBackendPostgres)
	}

	cfg.QdrantAddr = strings.TrimSpace(os.Getenv("DIDEROT_QDRANT_ADDR"))
	if cfg.QdrantAddr == "" {
		return nil, fmt.Errorf("DIDEROT_QDRANT_ADDR is required: the gRPC address of the Qdrant that holds the search index, such as qdrant:6334")
	}
	cfg.QdrantAPIKey = os.Getenv("DIDEROT_QDRANT_API_KEY")
	if raw := strings.TrimSpace(os.Getenv("DIDEROT_QDRANT_TLS")); raw != "" {
		useTLS, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("DIDEROT_QDRANT_TLS: %w", err)
		}
		cfg.QdrantTLS = useTLS
	}
	if cfg.QdrantRootCAs, err = qdrantRootCAs(cfg.QdrantTLS); err != nil {
		return nil, err
	}
	qdrantShards, err := getenvPositiveInt("DIDEROT_QDRANT_SHARD_NUMBER", 1)
	if err != nil || uint64(qdrantShards) > math.MaxUint32 {
		return nil, fmt.Errorf("DIDEROT_QDRANT_SHARD_NUMBER must be an integer between 1 and %d", uint64(math.MaxUint32))
	}
	qdrantReplicas, err := getenvPositiveInt("DIDEROT_QDRANT_REPLICATION_FACTOR", 1)
	if err != nil || uint64(qdrantReplicas) > math.MaxUint32 {
		return nil, fmt.Errorf("DIDEROT_QDRANT_REPLICATION_FACTOR must be an integer between 1 and %d", uint64(math.MaxUint32))
	}
	cfg.QdrantShardNumber = uint32(qdrantShards)
	cfg.QdrantReplicationFactor = uint32(qdrantReplicas)

	cfg.GatewayAPIKey = strings.TrimSpace(os.Getenv("DIDEROT_GATEWAY_API_KEY"))
	cfg.EmbedWorkers = getenvInt("DIDEROT_EMBED_WORKERS", 1)
	cfg.EmbedBatch = getenvInt("DIDEROT_EMBED_BATCH_SIZE", 32)

	if cfg.ConfigKeys, err = readKeyring("DIDEROT_CONFIG_KEYS", "DIDEROT_CONFIG_KEYS_FILE"); err != nil {
		return nil, err
	}

	cfg.PublicURL = strings.TrimRight(strings.TrimSpace(os.Getenv("DIDEROT_PUBLIC_URL")), "/")
	if cfg.PublicURL == "" {
		return nil, fmt.Errorf("DIDEROT_PUBLIC_URL is required: the address people open Diderot at, such as https://diderot.example.com")
	}
	publicURL, err := url.Parse(cfg.PublicURL)
	if err != nil || publicURL.Host == "" || publicURL.User != nil ||
		(publicURL.Scheme != "http" && publicURL.Scheme != "https") ||
		(publicURL.Path != "" && publicURL.Path != "/") || publicURL.RawQuery != "" || publicURL.Fragment != "" {
		return nil, fmt.Errorf("DIDEROT_PUBLIC_URL must be an http(s) origin without credentials, path, query, or fragment")
	}

	if err := loadAuth(cfg); err != nil {
		return nil, err
	}

	cfg.AskEnabled = os.Getenv("DIDEROT_ASK_ENABLED") == "true"
	cfg.AskReadinessTimeout = getenvDuration("DIDEROT_ASK_READINESS_TIMEOUT", 5*time.Second)
	cfg.AskReadinessInterval = getenvDuration("DIDEROT_ASK_READINESS_INTERVAL", 30*time.Second)
	cfg.ImagesBucket = os.Getenv("DIDEROT_IMAGES_BUCKET")
	cfg.ImagesRegion = os.Getenv("DIDEROT_IMAGES_REGION")
	cfg.ImagesEndpoint = strings.TrimSpace(os.Getenv("DIDEROT_IMAGES_ENDPOINT"))
	if cfg.ImagesEndpoint != "" {
		u, err := url.Parse(cfg.ImagesEndpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return nil, fmt.Errorf("DIDEROT_IMAGES_ENDPOINT must be an http(s) URL without credentials, query, or fragment")
		}
	}
	if raw := strings.TrimSpace(os.Getenv("DIDEROT_IMAGES_PATH_STYLE")); raw != "" {
		pathStyle, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("DIDEROT_IMAGES_PATH_STYLE: %w", err)
		}
		cfg.ImagesPathStyle = pathStyle
	}
	cfg.CaptionWorkers = getenvInt("DIDEROT_CAPTION_WORKERS", 0)
	cfg.DocExtractWorkers = getenvInt("DIDEROT_DOC_EXTRACT_WORKERS", 0)

	// Session ingest (internal/sessioningest). The default policy lives
	// in-code (see the sessioningest package); these envs let tests + a
	// mounted policy override.
	cfg.SessionIngestTick = getenvDuration("DIDEROT_SESSION_INGEST_TICK", 30*time.Second)
	cfg.SessionIngestPageSize = getenvInt("DIDEROT_SESSION_INGEST_PAGE_SIZE", 0)
	cfg.SessionIngestBatchSize = getenvInt("DIDEROT_SESSION_INGEST_BATCH_SIZE", 0)
	cfg.SessionIngestConcurrency = getenvInt("DIDEROT_SESSION_INGEST_CONCURRENCY", 1)
	cfg.SessionIngestChunkConcurrency, err = getenvPositiveInt("DIDEROT_SESSION_INGEST_CHUNK_CONCURRENCY", 20)
	if err != nil {
		return nil, err
	}
	cfg.SessionIngestChunkBatchSize = getenvInt("DIDEROT_SESSION_INGEST_CHUNK_BATCH_SIZE", 0)
	cfg.SessionIngestCodexApp = os.Getenv("DIDEROT_SESSION_INGEST_CODEX_APP")
	cfg.SessionIngestExcludeApp = os.Getenv("DIDEROT_SESSION_INGEST_EXCLUDE_APP")
	cfg.SessionIngestPolicyPath = os.Getenv("DIDEROT_SESSION_INGEST_POLICY_PATH")
	// Bounded post-decompression revision cap. Session ingest refuses to
	// read the session feed without it (cmd/diderot/session_ingest.go).
	cfg.SessionIngestMaxRevisionBytes = int64(getenvInt("DIDEROT_SESSION_INGEST_MAX_REVISION_BYTES", 0))
	if err := loadSessionPush(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// requiredGroups reads a comma-separated group allowlist that has no
// default: which groups may do what differs at every organization. A value
// with no group names in it (",") admits no one.
func requiredGroups(name, purpose string) ([]string, error) {
	raw := os.Getenv(name)
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%s is required: %s, comma-separated (\",\" admits no one)", name, purpose)
	}
	var groups []string
	for _, group := range strings.Split(raw, ",") {
		if group = strings.TrimSpace(group); group != "" {
			groups = append(groups, group)
		}
	}
	return groups, nil
}

// qdrantRootCAs reads DIDEROT_QDRANT_CA_FILE, the PEM certificates of the CAs
// Qdrant's certificate must chain to in place of the system's. Unset, it
// returns nil.
func qdrantRootCAs(useTLS bool) (*x509.CertPool, error) {
	path := strings.TrimSpace(os.Getenv("DIDEROT_QDRANT_CA_FILE"))
	if path == "" {
		return nil, nil
	}
	if !useTLS {
		return nil, fmt.Errorf("DIDEROT_QDRANT_CA_FILE requires DIDEROT_QDRANT_TLS=true")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("DIDEROT_QDRANT_CA_FILE cannot be read: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(contents) {
		return nil, fmt.Errorf("DIDEROT_QDRANT_CA_FILE %s holds no PEM certificates", path)
	}
	return pool, nil
}

func getenvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func getenvPositiveInt(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}
