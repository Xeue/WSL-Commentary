package kvs

// cognito.go is the AWS-facing half of the KVS credential chain: turning an
// m2lx.KVSToken into temporary AWS credentials via Cognito
// GetCredentialsForIdentity (step 3 of the chain documented in kvs.go). It is
// kept as its own file, behind the small cognitoClient interface below, so
// that if the measured response shape (docs/test-results.md around line 141)
// turns out wrong against a live instance — a field renamed, a type changed —
// fixing it is a one-file change that never touches Fetch's exported
// signature or the Credentials JSON contract WP-5a depends on.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentity"

	"wslcomms/internal/m2lx"
)

// cognitoClient is the subset of *cognitoidentity.Client this package calls.
// *cognitoidentity.Client satisfies it structurally with no adapter needed;
// declaring it as an interface here lets tests substitute a fake so the whole
// Fetch chain can be exercised without a network call or AWS credentials in
// the test environment.
type cognitoClient interface {
	GetCredentialsForIdentity(ctx context.Context, params *cognitoidentity.GetCredentialsForIdentityInput, optFns ...func(*cognitoidentity.Options)) (*cognitoidentity.GetCredentialsForIdentityOutput, error)
}

// cognitoDialer constructs the cognitoClient used for the token exchange, for
// the AWS region carried in the webrtc_info response. It is the seam fetch's
// tests use to substitute a fake; production code always passes dialCognito.
type cognitoDialer func(ctx context.Context, region string) (cognitoClient, error)

// dialCognito is the production cognitoDialer. It loads the AWS SDK's default
// config scoped to region — the region measured in webrtc_info ("eu-west-1"
// in the one sample on record, docs/test-results.md around line 141), never a
// hardcoded value, because a different event could live in a different
// region.
//
// GetCredentialsForIdentity is a public, unauthenticated Cognito Identity
// operation: aws-sdk-go-v2's generated auth resolver maps it to the anonymous
// auth scheme unconditionally (cognitoidentity's auth.go,
// operationAuthOptions["GetCredentialsForIdentity"]), so the call succeeds
// even when LoadDefaultConfig finds no AWS credentials in the environment at
// all — which is the expected case for this app; it never has its own AWS
// account credentials, only what M2L-X's webrtc_token hands it.
func dialCognito(ctx context.Context, region string) (cognitoClient, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("kvs: loading AWS SDK config for region %q: %w", region, err)
	}
	return cognitoidentity.NewFromConfig(cfg), nil
}

// exchangeCredentials runs step 3 of the chain: it presents tok's identity and
// token to Cognito and maps the response onto Credentials. region and
// channelName come from the already-validated webrtc_info response (see
// validateChannelName in kvs.go) and pass straight through to the result.
//
// Every field of the Cognito response this package relies on is checked
// individually so a missing or blank field is reported by name rather than
// surfacing as a zero-valued Credentials that fails mysteriously later. The
// token and the credential values themselves are never included in an error
// string; tok.IdentityID is not a secret (it is a Cognito identity id in the
// form "region:guid") and is included for diagnosis.
func exchangeCredentials(ctx context.Context, cc cognitoClient, region, channelName string, tok m2lx.KVSToken) (Credentials, error) {
	loginsKey := tok.LoginsKey
	if loginsKey == "" {
		loginsKey = DefaultLoginsKey
	}

	out, err := cc.GetCredentialsForIdentity(ctx, &cognitoidentity.GetCredentialsForIdentityInput{
		IdentityId: awssdk.String(tok.IdentityID),
		Logins:     map[string]string{loginsKey: tok.Token},
	})
	if err != nil {
		return Credentials{}, fmt.Errorf("kvs: cognito GetCredentialsForIdentity for identity %q: %w", tok.IdentityID, err)
	}
	if out == nil || out.Credentials == nil {
		return Credentials{}, errors.New("kvs: cognito GetCredentialsForIdentity response had no \"Credentials\" field")
	}

	cr := out.Credentials
	var missing []string
	if cr.AccessKeyId == nil || *cr.AccessKeyId == "" {
		missing = append(missing, "AccessKeyId")
	}
	if cr.SecretKey == nil || *cr.SecretKey == "" {
		missing = append(missing, "SecretKey")
	}
	if cr.SessionToken == nil || *cr.SessionToken == "" {
		missing = append(missing, "SessionToken")
	}
	if cr.Expiration == nil {
		missing = append(missing, "Expiration")
	}
	if len(missing) > 0 {
		return Credentials{}, fmt.Errorf("kvs: cognito GetCredentialsForIdentity response missing field(s): %s", strings.Join(missing, ", "))
	}

	return Credentials{
		Region:       region,
		ChannelName:  channelName,
		AccessKeyID:  *cr.AccessKeyId,
		SecretKey:    *cr.SecretKey,
		SessionToken: *cr.SessionToken,
		Expiry:       *cr.Expiration,
	}, nil
}
