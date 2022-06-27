package main

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
	"os"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/swag"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/in-toto/in-toto-golang/in_toto"
	fapi "github.com/sigstore/fulcio/pkg/api"
	rekor "github.com/sigstore/rekor/pkg/client"
	rentries "github.com/sigstore/rekor/pkg/generated/client/entries"
	rindex "github.com/sigstore/rekor/pkg/generated/client/index"
	rmodels "github.com/sigstore/rekor/pkg/generated/models"
	"github.com/sigstore/sigstore/pkg/fulcioroots"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/dsse"
)

const (
	aud           = "sigstore"
	fulcioURL     = "https://fulcio.sigstore.dev"
	fulcioTimeout = time.Minute
	rekorURL      = "https://rekor.sigstore.dev"
	rekorTimeout  = time.Minute
)

var sub = email()

func email() string {
	email, err := getMetadata("http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/email")
	if err != nil {
		log.Fatalf("failed to get email: %v", err)
	}
	log.Println("Hello, my name is", email)
	return email
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
	return getMetadata("http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=" + aud)
}

// TODO(jason): Include Rekor tlog information in response headers.
func rekorPut(ctx context.Context, w io.Writer, tag name.Tag, digest string) error {
	idtoken, err := idtoken(ctx)
	if err != nil {
		return err
	}

	// Get signing cert from ephemeral private key and idtoken.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating ephemeral private key: %w", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return err
	}
	h := sha256.Sum256([]byte(sub))
	proof, err := ecdsa.SignASN1(rand.Reader, priv, h[:])
	if err != nil {
		return err
	}
	fulcioServer, err := neturl.Parse(fulcioURL)
	if err != nil {
		return fmt.Errorf("creating Fulcio client: %w", err)
	}
	fclient := fapi.NewClient(fulcioServer)
	fresp, err := fclient.SigningCert(fapi.CertificateRequest{
		PublicKey: fapi.Key{
			Algorithm: "ecdsa",
			Content:   pubBytes,
		},
		SignedEmailAddress: proof,
	}, idtoken)
	if err != nil {
		return fmt.Errorf("getting signing cert: %w", err)
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
		return fmt.Errorf("encoding message: %w", err)
	}
	s, err := signature.LoadECDSASigner(priv, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("loading signer: %w", err)
	}
	signed, err := dsse.WrapSigner(s, in_toto.PayloadType).SignMessage(bytes.NewReader(msg))
	if err != nil {
		return fmt.Errorf("signing: %w", err)
	}
	log.Println("FULCIO CERT:\n", string(fresp.CertPEM)) // TODO remove

	// Record tag + digest, with ephemeral Fulcio cert as private key.
	certPEMBase64 := strfmt.Base64(fresp.CertPEM)
	params := rentries.NewCreateLogEntryParams()
	params.SetTimeout(fulcioTimeout)
	params.SetProposedEntry(&rmodels.Intoto{
		APIVersion: swag.String("0.0.1"),
		Spec: rmodels.IntotoV001Schema{
			Content: &rmodels.IntotoV001SchemaContent{
				Envelope: string(signed),
			},
			PublicKey: &certPEMBase64,
		},
	})
	client, err := rekor.GetRekorClient(rekorURL)
	if err != nil {
		return fmt.Errorf("creating rekor client: %w", err)
	}
	created, err := client.Entries.CreateLogEntry(params)
	if err != nil {
		return fmt.Errorf("adding Rekor entry: %w", err)
	}

	le := created.Payload[created.ETag]
	log.Println("---- Rekor entry created!")
	log.Println("- UUID:", created.ETag)
	log.Println("- Integrated Time:", time.Unix(*le.IntegratedTime, 0).Format(time.RFC3339))
	log.Println("- Log Index:", *le.LogIndex)
	leb, err := base64.StdEncoding.DecodeString(le.Body.(string))
	if err != nil {
		return fmt.Errorf("decoding Rekor LogEntry body: %w", err)
	}
	log.Println("- Entry:", string(leb))

	fmt.Fprintln(w, "uuid", created.ETag)
	fmt.Fprintln(w, "log index", *le.LogIndex)
	fmt.Fprintln(w, "entry:", string(leb))
	return nil
}

// rekorGet searches Rekor for entries associated with the given tag, and
// returns all digests attested to by those entries, signed by a Fulcio cert
// associated with our identity.
func rekorGet(ctx context.Context, tag name.Tag) (digest string, err error) {
	// Get Fulcio root cert.
	fulcioRoot, err := fulcioroots.Get()
	if err != nil {
		return "", fmt.Errorf("failed getting Fulcio root cert")
	}
	fulcioIntermediates, err := fulcioroots.GetIntermediates()
	if err != nil {
		return "", fmt.Errorf("failed getting Fulcio intermedate certs")
	}

	// Find entries for digest of fully qualified tagged image ref.
	client, err := rekor.GetRekorClient(rekorURL)
	if err != nil {
		return "", fmt.Errorf("creating rekor client: %w", err)
	}
	iparams := rindex.NewSearchIndexParams()
	iparams.SetTimeout(rekorTimeout)
	iparams.SetQuery(&rmodels.SearchIndex{Hash: fmt.Sprintf("%x", sha256.Sum256([]byte(tag.String())))}) // Search by the digest of the tag.
	iresp, err := client.Index.SearchIndex(iparams)
	if err != nil {
		return "", fmt.Errorf("querying Rekor entries: %w", err)
	}
	if len(iresp.Payload) == 0 {
		return "", nil // Never seen this image:tag before.
	}
	found := map[string]struct{}{} // unique digests from verified attestations.
	for _, e := range iresp.Payload {
		gparams := rentries.NewGetLogEntryByUUIDParams()
		gparams.SetTimeout(rekorTimeout)
		gparams.SetEntryUUID(e)
		gresp, err := client.Entries.GetLogEntryByUUID(gparams)
		if err != nil {
			return "", fmt.Errorf("getting Rekor entry %q: %w", e, err)
		}

		enc := json.NewEncoder(os.Stderr)
		enc.SetIndent("", "  ")
		enc.Encode(gresp) // TODO remove

		le := gresp.Payload[e]
		if le.Body == nil {
			log.Println("No body for entry:", e)
			continue
		}
		leb, err := base64.StdEncoding.DecodeString(le.Body.(string))
		if err != nil {
			return "", fmt.Errorf("decoding Rekor LogEntry body: %w", err)
		}
		var ent struct {
			Spec struct {
				PublicKey []byte
			}
		}
		if err := json.Unmarshal(leb, &ent); err != nil {
			return "", fmt.Errorf("unmarshaling Rekor LogEntry body: %w", err)
		}

		if len(ent.Spec.PublicKey) == 0 {
			continue // Entry doesn't have a public key attached.
		}
		block, _ := pem.Decode(ent.Spec.PublicKey)
		if block == nil {
			log.Printf("decoding %q: no PEM block found", e)
			continue // Entry's public key isn't PEM-encoded
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			log.Printf("decoding %q: parsing certificate: %v", e, err)
			continue // Entry's pubclic key isn't a valid X.509 cert.
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

		if cert.EmailAddresses[0] != sub {
			// Ignore entries not recorded by us.
			// Don't log this since it may be spammy and doesn't matter.
			continue
		}
	}

	switch len(found) {
	case 0:
		return "", nil // No entries found for tag.
	case 1:
		for d := range found {
			return d, nil
		}
	}
	return "", fmt.Errorf("multiple digests found for %s: %v", tag, found)
}
