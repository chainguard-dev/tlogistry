package rekor

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"sync"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/swag"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/in-toto/in-toto-golang/in_toto"
	"github.com/kelseyhightower/envconfig"
	fapi "github.com/sigstore/fulcio/pkg/api"
	rekor "github.com/sigstore/rekor/pkg/client"
	"github.com/sigstore/rekor/pkg/generated/client"
	rentries "github.com/sigstore/rekor/pkg/generated/client/entries"
	rindex "github.com/sigstore/rekor/pkg/generated/client/index"
	rmodels "github.com/sigstore/rekor/pkg/generated/models"
	"github.com/sigstore/sigstore/pkg/fulcioroots"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/dsse"
)

var rekorClient *client.Rekor
var fulcioClient fapi.LegacyClient

var env struct {
	Audience      string        `envconfig:"AUDIENCE" default:"sigstore"`
	RekorURL      string        `envconfig:"REKOR_URL" default:"https://rekor.sigstore.dev"`
	FulcioURL     string        `envconfig:"FULCIO_URL" default:"https://fulcio.sigstore.dev"`
	FulcioTimeout time.Duration `envconfig:"FULCIO_TIMEOUT" default:"1m"`
	RekorTimeout  time.Duration `envconfig:"REKOR_TIMEOUT" default:"1m"`
}

func init() {
	if err := envconfig.Process("", &env); err != nil {
		log.Fatalf("envconfig: %v", err)
	}

	var err error
	rekorClient, err = rekor.GetRekorClient(env.RekorURL)
	if err != nil {
		log.Fatalf("creating rekor client: %v", err)
	}
	fulcioServer, err := neturl.Parse(env.FulcioURL)
	if err != nil {
		log.Fatalf("creating Fulcio client: %v", err)
	}
	fulcioClient = fapi.NewClient(fulcioServer)
}

var internalEmail string
var emailOnce sync.Once

func email() string {
	emailOnce.Do(func() {
		email, err := getMetadata("http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/email")
		if err != nil {
			log.Fatalf("failed to get email: %v", err)
		}
		log.Println("Hello, my name is", email)
		internalEmail = email
	})
	return internalEmail
}

func getMetadata(url string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	all, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d: %s", resp.StatusCode, string(all))
	}
	return string(all), nil
}

func idtoken(ctx context.Context) (idtoken string, err error) {
	return getMetadata("http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=" + env.Audience)
}

// Info represents information found in Rekor about the tag.
type Info struct {
	UUID           string
	LogIndex       int64
	IntegratedTime time.Time
}

// Put adds a new entry to the log.
func Put(ctx context.Context, tag name.Tag, digest string) (*Info, error) {
	idtoken, err := idtoken(ctx)
	if err != nil {
		return nil, err
	}

	// Get signing cert from ephemeral private key and idtoken.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating ephemeral private key: %w", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshaling public key: %w", err)
	}
	h := sha256.Sum256([]byte(email()))
	proof, err := ecdsa.SignASN1(rand.Reader, priv, h[:])
	if err != nil {
		return nil, fmt.Errorf("signing identity with private key: %w", err)
	}
	fresp, err := fulcioClient.SigningCert(fapi.CertificateRequest{
		PublicKey: fapi.Key{
			Algorithm: "ecdsa",
			Content:   pubBytes,
		},
		SignedEmailAddress: proof,
	}, idtoken)
	if err != nil {
		return nil, fmt.Errorf("getting signing cert: %w", err)
	}

	// Sign the message.
	msg, err := json.Marshal(in_toto.Statement{
		StatementHeader: in_toto.StatementHeader{
			Type:          "intoto",
			PredicateType: "tlogistry-fetched",
			Subject: []in_toto.Subject{{
				Name:   tag.String(),
				Digest: map[string]string{"sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(tag.String())))},
			}},
		},
		Predicate: map[string]string{
			"tag":    tag.String(),
			"digest": digest,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encoding message: %w", err)
	}
	s, err := signature.LoadECDSASigner(priv, crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("loading signer: %w", err)
	}
	signed, err := dsse.WrapSigner(s, in_toto.PayloadType).SignMessage(bytes.NewReader(msg))
	if err != nil {
		return nil, fmt.Errorf("signing message: %w", err)
	}

	// Record tag + digest, with ephemeral Fulcio cert as private key.
	certPEMBase64 := strfmt.Base64(fresp.CertPEM)
	params := rentries.NewCreateLogEntryParams()
	params.SetTimeout(env.FulcioTimeout)
	params.SetProposedEntry(&rmodels.Intoto{
		APIVersion: swag.String("0.0.1"),
		Spec: rmodels.IntotoV001Schema{
			Content: &rmodels.IntotoV001SchemaContent{
				Envelope: string(signed),
			},
			PublicKey: &certPEMBase64,
		},
	})
	created, err := rekorClient.Entries.CreateLogEntry(params)
	if err != nil {
		return nil, fmt.Errorf("adding Rekor entry: %w", err)
	}

	le := created.Payload[created.ETag]
	log.Println("---- Rekor entry created!")
	log.Println("- UUID:", created.ETag)
	log.Println("- Integrated Time:", time.Unix(*le.IntegratedTime, 0).Format(time.RFC3339))
	log.Println("- Log Index:", *le.LogIndex)
	leb, err := base64.StdEncoding.DecodeString(le.Body.(string))
	if err != nil {
		return nil, fmt.Errorf("decoding Rekor LogEntry body: %w", err)
	}
	log.Println("- Entry:", string(leb))
	return &Info{
		UUID:           created.ETag,
		LogIndex:       *le.LogIndex,
		IntegratedTime: time.Unix(*le.IntegratedTime, 0),
	}, nil
}

// Get searches Rekor for entries associated with the given tag, and
// returns all digests attested to by those entries, signed by a Fulcio cert
// associated with our identity.
func Get(ctx context.Context, tag name.Tag) (string, *Info, error) {
	// Get Fulcio root cert.
	fulcioRoot, err := fulcioroots.Get()
	if err != nil {
		return "", nil, fmt.Errorf("getting Fulcio root cert: %w", err)
	}
	fulcioIntermediates, err := fulcioroots.GetIntermediates()
	if err != nil {
		return "", nil, fmt.Errorf("getting Fulcio intermedate certs: %w", err)
	}

	// Find entries for digest of fully qualified tagged image ref.
	iparams := rindex.NewSearchIndexParams()
	iparams.SetTimeout(env.RekorTimeout)
	iparams.SetQuery(&rmodels.SearchIndex{Hash: fmt.Sprintf("%x", sha256.Sum256([]byte(tag.String())))}) // Search by the digest of the tag.
	iresp, err := rekorClient.Index.SearchIndex(iparams)
	if err != nil {
		return "", nil, fmt.Errorf("querying Rekor entries: %w", err)
	}
	if len(iresp.Payload) == 0 {
		return "", nil, nil // Never seen this image:tag before.
	}
	found := map[string]*Info{} // unique digests from verified attestations.
	for _, e := range iresp.Payload {
		log.Println("- matched found Rekor entry:", e)
		gparams := rentries.NewGetLogEntryByUUIDParams()
		gparams.SetTimeout(env.RekorTimeout)
		gparams.SetEntryUUID(e)
		gresp, err := rekorClient.Entries.GetLogEntryByUUID(gparams)
		if err != nil {
			log.Printf("error getting Rekor entry: %v", err)
			continue
		}

		var le rmodels.LogEntryAnon
		if len(gresp.Payload) != 1 {
			log.Println("unexpected payloads:", gresp.Payload)
			continue
		}
		for _, v := range gresp.Payload {
			le = v
			break
		}
		if le.Body == nil {
			log.Println("No body for entry:", e)
			continue
		}

		var att struct {
			PredicateType string `json:"predicateType"`
			Predicate     struct {
				Digest string `json:"digest"`
				Tag    string `json:"tag"`
			}
		}
		if err := json.Unmarshal(le.Attestation.Data, &att); err != nil {
			log.Printf("json-decoding Rekor LogEntry attestation data: %v", err)
			continue
		}
		if att.PredicateType != "tlogistry-fetched" {
			log.Printf("Rekor LogEntry attestation predicateType %q not supported", att.PredicateType)
			continue
		}
		if att.Predicate.Tag != tag.String() {
			log.Printf("Rekor LogEntry predicate tag mismatch: got %q, want %q", att.Predicate.Tag, tag.String())
			continue // How did this even happen.
		}
		// Okay, we found an attestation for the tag in Rekor. Let's make sure it was put there by us.

		leb, err := base64.StdEncoding.DecodeString(le.Body.(string))
		if err != nil {
			return "", nil, fmt.Errorf("decoding Rekor LogEntry body: %w", err)
		}
		var ent struct {
			Spec struct {
				PublicKey []byte
			}
		}
		if err := json.Unmarshal(leb, &ent); err != nil {
			return "", nil, fmt.Errorf("unmarshaling Rekor LogEntry body: %w", err)
		}

		if len(ent.Spec.PublicKey) == 0 {
			log.Printf("public key is missing")
			continue
		}
		block, _ := pem.Decode(ent.Spec.PublicKey)
		if block == nil {
			log.Printf("decoding %q: no PEM block found", e)
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			log.Printf("decoding %q: parsing certificate: %v", e, err)
			continue
		}

		// Verify cert is from Fulcio.
		if _, err := cert.Verify(x509.VerifyOptions{
			// THIS IS IMPORTANT: WE DO NOT CHECK TIMES HERE
			// THE CERTIFICATE IS TREATED AS TRUSTED FOREVER
			// WE CHECK THAT THE SIGNATURES WERE CREATED DURING THIS WINDOW
			CurrentTime:   cert.NotBefore,
			Roots:         fulcioRoot,
			Intermediates: fulcioIntermediates,
			KeyUsages: []x509.ExtKeyUsage{
				x509.ExtKeyUsageCodeSigning,
			},
		}); err != nil {
			log.Printf("decoding %q: cert is not from Fulcio: %v", e, err)
			continue
		}

		if len(cert.EmailAddresses) != 1 {
			log.Printf("decoding %q: saw unexpected number of associated identities: %v", e, cert.EmailAddresses)
			continue
		}

		if cert.EmailAddresses[0] != email() {
			log.Printf("decoding %q: saw unexpected associated identity: %v", e, cert.EmailAddresses[0])
			// Ignore entries not recorded by us.
			// Don't log this since it may be spammy and doesn't matter.
			continue
		}

		log.Printf("found matching Rekor entry: %q", e)
		found[att.Predicate.Digest] = &Info{
			UUID:           e,
			LogIndex:       *le.LogIndex,
			IntegratedTime: time.Unix(*le.IntegratedTime, 0),
		}
	}

	switch len(found) {
	case 0:
		log.Println("no matching Rekor entries found for", tag)
		return "", nil, nil // No entries found for tag.
	case 1:
		for d, info := range found {
			return d, info, nil
		}
	}
	return "", nil, fmt.Errorf("multiple digests found for %s: %v", tag, found)
}
