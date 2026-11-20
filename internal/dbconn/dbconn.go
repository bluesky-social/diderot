// Package dbconn opens diderot's Postgres pool and builds the URL the
// migration runner uses.
//
// With IAM auth on (DIDEROT_DB_IAM_AUTH=true), the URL carries a user but no
// password: every new connection logs in with a fresh RDS IAM token signed
// with the process's AWS credentials (EKS Pod Identity, for example). A token
// is valid for 15 minutes and is checked only at login, so open connections
// are not affected when it expires. The database user is the one the URL
// names; it needs the rds_iam role, and the credentials' IAM policy must
// allow rds-db:connect as that user.
//
// A token is a bearer credential for its lifetime, so IAM mode refuses any
// TLS setting that doesn't verify the server's certificate and hostname
// (sslmode=verify-full).
package dbconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Options selects how diderot logs in to Postgres.
type Options struct {
	// URL is the Postgres connection URL. In IAM mode it names the user and
	// carries no password.
	URL string
	// IAMAuth logs in with RDS IAM tokens instead of the URL's password.
	IAMAuth bool
	// AWS overrides the default AWS config (tests). Nil loads the default
	// credential chain.
	AWS *aws.Config
}

// PoolConfig parses opts.URL into a pool config. In IAM mode it checks TLS and
// installs a BeforeConnect hook that sets a fresh token on every new
// connection.
func PoolConfig(ctx context.Context, opts Options) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DB URL: %w", err)
	}
	if !opts.IAMAuth {
		return cfg, nil
	}
	if err := checkIAMConnConfig(cfg.ConnConfig); err != nil {
		return nil, err
	}
	signer, err := newSigner(ctx, cfg.ConnConfig.Host, opts.AWS)
	if err != nil {
		return nil, err
	}
	cfg.BeforeConnect = func(ctx context.Context, cc *pgx.ConnConfig) error {
		token, err := signer.token(ctx, cc.Host, cc.Port, cc.User)
		if err != nil {
			return err
		}
		cc.Password = token
		return nil
	}
	return cfg, nil
}

// NewPool opens a pool with PoolConfig's settings.
func NewPool(ctx context.Context, opts Options) (*pgxpool.Pool, error) {
	cfg, err := PoolConfig(ctx, opts)
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// MigrationURL returns the URL for the migration runner (a subprocess that
// reads its credentials from the URL). In IAM mode it carries a fresh token as
// its password, valid for 15 minutes.
func MigrationURL(ctx context.Context, opts Options) (string, error) {
	if !opts.IAMAuth {
		return opts.URL, nil
	}
	cfg, err := pgx.ParseConfig(opts.URL)
	if err != nil {
		return "", fmt.Errorf("parse DB URL: %w", err)
	}
	if err := checkIAMConnConfig(cfg); err != nil {
		return "", err
	}
	u, err := url.Parse(opts.URL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("DB IAM auth needs DIDEROT_DB_URL in postgres:// URL form")
	}
	signer, err := newSigner(ctx, cfg.Host, opts.AWS)
	if err != nil {
		return "", err
	}
	token, err := signer.token(ctx, cfg.Host, cfg.Port, cfg.User)
	if err != nil {
		return "", err
	}
	// A password= query parameter would take precedence over the userinfo.
	query := u.Query()
	query.Del("password")
	u.RawQuery = query.Encode()
	u.User = url.UserPassword(cfg.User, token)
	return u.String(), nil
}

// checkIAMConnConfig refuses URLs that would send a token somewhere unverified
// or that already carry a password.
func checkIAMConnConfig(cc *pgx.ConnConfig) error {
	if cc.User == "" {
		return errors.New("DB IAM auth needs a user in DIDEROT_DB_URL")
	}
	if cc.Password != "" {
		return errors.New("DB IAM auth: DIDEROT_DB_URL must not carry a password")
	}
	if cc.TLSConfig == nil || cc.TLSConfig.InsecureSkipVerify || len(cc.Fallbacks) > 0 {
		return errors.New("DB IAM auth needs sslmode=verify-full in DIDEROT_DB_URL")
	}
	return nil
}

type signer struct {
	region string
	creds  aws.CredentialsProvider
}

func newSigner(ctx context.Context, host string, override *aws.Config) (*signer, error) {
	var cfg aws.Config
	if override != nil {
		cfg = *override
	} else {
		loaded, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("DB IAM auth: load AWS config: %w", err)
		}
		cfg = loaded
	}
	region := regionFromHost(host)
	if region == "" {
		region = cfg.Region
	}
	if region == "" {
		return nil, fmt.Errorf("DB IAM auth: no AWS region for %q", host)
	}
	if cfg.Credentials == nil {
		return nil, errors.New("DB IAM auth: no AWS credentials")
	}
	return &signer{region: region, creds: cfg.Credentials}, nil
}

func (s *signer) token(ctx context.Context, host string, port uint16, user string) (string, error) {
	endpoint := net.JoinHostPort(host, strconv.Itoa(int(port)))
	token, err := auth.BuildAuthToken(ctx, endpoint, s.region, user, s.creds)
	if err != nil {
		return "", fmt.Errorf("DB IAM auth: build token for %s@%s: %w", user, endpoint, err)
	}
	return token, nil
}

// regionFromHost reads the region from an RDS endpoint
// (<name>.<id>.<region>.rds.amazonaws.com); "" for any other host.
func regionFromHost(host string) string {
	if !strings.HasSuffix(host, ".rds.amazonaws.com") {
		return ""
	}
	parts := strings.Split(host, ".")
	if len(parts) < 5 {
		return ""
	}
	return parts[len(parts)-4]
}
