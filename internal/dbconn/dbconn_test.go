package dbconn

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

const rdsHost = "diderot.c0example0000.us-east-2.rds.amazonaws.com"

func testAWS() *aws.Config {
	return &aws.Config{Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "session")}
}

func TestRegionFromHost(t *testing.T) {
	require.Equal(t, "us-east-2", regionFromHost(rdsHost))
	require.Equal(t, "", regionFromHost("localhost"))
	require.Equal(t, "", regionFromHost("db.example.com"))
}

func TestPasswordModeUnchanged(t *testing.T) {
	const raw = "postgres://diderot_app:pw@localhost:5432/diderot?sslmode=disable"
	cfg, err := PoolConfig(context.Background(), Options{URL: raw})
	require.NoError(t, err)
	require.Nil(t, cfg.BeforeConnect)
	require.Equal(t, "pw", cfg.ConnConfig.Password)

	got, err := MigrationURL(context.Background(), Options{URL: raw})
	require.NoError(t, err)
	require.Equal(t, raw, got)
}

func TestIAMPoolSetsTokenPerConnection(t *testing.T) {
	raw := "postgres://diderot@" + rdsHost + ":5432/diderot?sslmode=verify-full"
	cfg, err := PoolConfig(context.Background(), Options{URL: raw, IAMAuth: true, AWS: testAWS()})
	require.NoError(t, err)
	require.NotNil(t, cfg.BeforeConnect)

	cc := cfg.ConnConfig.Copy()
	require.NoError(t, cfg.BeforeConnect(context.Background(), cc))
	require.True(t, strings.HasPrefix(cc.Password, rdsHost+":5432?"), cc.Password)
	require.Contains(t, cc.Password, "Action=connect")
	require.Contains(t, cc.Password, "DBUser=diderot")
	require.Contains(t, cc.Password, "us-east-2")
}

func TestIAMMigrationURLCarriesToken(t *testing.T) {
	// The repo's copy of the bundle the image ships at /etc/diderot.
	raw := "postgres://diderot@" + rdsHost + ":5432/diderot?sslmode=verify-full&sslrootcert=../../certs/rds-global-bundle.pem"
	got, err := MigrationURL(context.Background(), Options{URL: raw, IAMAuth: true, AWS: testAWS()})
	require.NoError(t, err)

	u, err := url.Parse(got)
	require.NoError(t, err)
	require.Equal(t, "diderot", u.User.Username())
	pw, ok := u.User.Password()
	require.True(t, ok)
	require.Contains(t, pw, "DBUser=diderot")
	require.Equal(t, "verify-full", u.Query().Get("sslmode"))

	// An empty password= query parameter must not override the token.
	withEmpty, err := MigrationURL(context.Background(), Options{URL: raw + "&password=", IAMAuth: true, AWS: testAWS()})
	require.NoError(t, err)
	emptyCC, err := pgx.ParseConfig(withEmpty)
	require.NoError(t, err)
	require.Contains(t, emptyCC.Password, "DBUser=diderot")

	// The token survives the round trip through the URL intact.
	cc, err := pgx.ParseConfig(got)
	require.NoError(t, err)
	require.Equal(t, pw, cc.Password)
}

func TestIAMRefusesUnverifiedTLSAndPasswords(t *testing.T) {
	bad := map[string]string{
		"require":     "postgres://diderot@" + rdsHost + ":5432/diderot?sslmode=require",
		"verify-ca":   "postgres://diderot@" + rdsHost + ":5432/diderot?sslmode=verify-ca",
		"prefer":      "postgres://diderot@" + rdsHost + ":5432/diderot?sslmode=prefer",
		"disable":     "postgres://diderot@" + rdsHost + ":5432/diderot?sslmode=disable",
		"password":    "postgres://diderot:pw@" + rdsHost + ":5432/diderot?sslmode=verify-full",
		"keyword dsn": "host=" + rdsHost + " user=diderot dbname=diderot sslmode=verify-full",
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			if name != "keyword dsn" {
				_, err := PoolConfig(context.Background(), Options{URL: raw, IAMAuth: true, AWS: testAWS()})
				require.Error(t, err)
			}
			_, err := MigrationURL(context.Background(), Options{URL: raw, IAMAuth: true, AWS: testAWS()})
			require.Error(t, err)
		})
	}
}
