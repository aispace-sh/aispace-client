package cmd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aispace-sh/aispace-client/internal/api"
	identitypkg "github.com/aispace-sh/aispace-client/internal/identity"
)

const bindingDomain = "aispace-identity-encryption-binding-v1\x00"

type pendingIdentityCreate struct {
	Version                 int                        `json:"version"`
	ServerURL               string                     `json:"server_url"`
	Handle                  string                     `json:"handle"`
	DisplayName             string                     `json:"display_name"`
	ChallengeIdempotencyKey string                     `json:"challenge_idempotency_key"`
	CreateIdempotencyKey    string                     `json:"create_idempotency_key,omitempty"`
	KeyPath                 string                     `json:"key_path,omitempty"`
	Request                 *api.CreateIdentityRequest `json:"request,omitempty"`
}

type pendingIdentityRotation struct {
	Version        int                          `json:"version"`
	ServerURL      string                       `json:"server_url"`
	IdentityID     string                       `json:"identity_id"`
	SuccessorKeyID string                       `json:"successor_key_id"`
	Purpose        string                       `json:"purpose"`
	StagedPath     string                       `json:"staged_path"`
	IdempotencyKey string                       `json:"idempotency_key"`
	Request        api.RotateIdentityKeyRequest `json:"request"`
}

func (a *app) identityStore() (identitypkg.Store, error) {
	cfg, err := a.resolve()
	if err != nil {
		return identitypkg.Store{}, err
	}
	return identitypkg.NewStore(cfg.Path), nil
}

func (a *app) identityCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "identity", Short: "Manage local agent identity keys and their public records", Args: noArgs}
	cmd.AddCommand(a.identityCreateCmd(), a.identityListCmd(), a.identityShowCmd(), a.identityDisableCmd(), a.identityRotateCmd(), a.identityRevokeCmd())
	return cmd
}

func (a *app) identityCreateCmd() *cobra.Command {
	var name, handle string
	cmd := &cobra.Command{Use: "create --name NAME --handle SLUG", Short: "Create an agent identity with client-held encryption and signing keys", Args: noArgs, RunE: func(cmd *cobra.Command, args []string) error {
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 100 || !validHandle(handle) {
			return usagef("--name must be 1-100 bytes after trimming and --handle must be a lowercase slug")
		}
		client, err := a.client()
		if err != nil {
			return err
		}
		store, err := a.identityStore()
		if err != nil {
			return err
		}
		var pending pendingIdentityCreate
		pendingErr := store.LoadPending("identity-create", handle, &pending)
		if pendingErr == nil {
			if pending.Version != 1 || pending.ServerURL != client.BaseURL || pending.Handle != handle || pending.DisplayName != name || pending.ChallengeIdempotencyKey == "" {
				return errors.New("stored pending identity creation is invalid or belongs to another server/name")
			}
			if pending.Request != nil {
				return a.publishPendingIdentityCreate(cmd.Context(), client, store, pending)
			}
		} else if errors.Is(pendingErr, os.ErrNotExist) {
			pending = pendingIdentityCreate{Version: 1, ServerURL: client.BaseURL, Handle: handle, DisplayName: name, ChallengeIdempotencyKey: newIdempotencyKey()}
			if _, err := store.SavePending("identity-create", handle, pending); err != nil {
				return fmt.Errorf("cannot persist recoverable identity challenge: %w", err)
			}
		} else {
			return pendingErr
		}
		challenge, err := client.CreateIdentityChallenge(cmd.Context(), "identity.create", "", pending.ChallengeIdempotencyKey)
		if err != nil {
			return fmt.Errorf("identity challenge response was not recovered; exact replay state was retained: %w", err)
		}
		if challenge.Value.IdentityID == "" || challenge.Value.Challenge == "" {
			return errors.New("identity challenge omitted identity ID or challenge")
		}
		keys, err := identitypkg.GenerateKeyPair()
		if err != nil {
			return err
		}
		createdAt := challenge.Value.IssuedAt
		encID, sigID := newULID(createdAt), newULID(createdAt)
		enc := api.InitialIdentityKey{ID: encID, Algorithm: identitypkg.EncryptionAlgorithm, PublicKey: identitypkg.PublicKeyString(keys.EncryptionPublic), CreatedAt: createdAt}
		sign := api.InitialIdentityKey{ID: sigID, Algorithm: identitypkg.SigningAlgorithm, PublicKey: identitypkg.PublicKeyString(keys.SigningPublic), CreatedAt: createdAt}
		possession := struct {
			Challenge          string                 `json:"challenge"`
			ChallengeExpiresAt int64                  `json:"challenge_expires_at"`
			EncryptionKey      api.InitialIdentityKey `json:"encryption_key"`
			IdentityID         string                 `json:"identity_id"`
			SigningKey         api.InitialIdentityKey `json:"signing_key"`
		}{challenge.Value.Challenge, challenge.Value.ExpiresAt, enc, challenge.Value.IdentityID, sign}
		canonical, err := identitypkg.CanonicalJSON(possession)
		if err != nil {
			return err
		}
		posSig, err := identitypkg.SignCanonical(keys.SigningPrivate, identitypkg.PossessionDomain, canonical)
		if err != nil {
			return err
		}
		binding := struct {
			EncryptionKey api.InitialIdentityKey `json:"encryption_key"`
			IdentityID    string                 `json:"identity_id"`
			SigningKeyID  string                 `json:"signing_key_id"`
		}{enc, challenge.Value.IdentityID, sigID}
		canonical, err = identitypkg.CanonicalJSON(binding)
		if err != nil {
			return err
		}
		bindSig, err := identitypkg.SignCanonical(keys.SigningPrivate, bindingDomain, canonical)
		if err != nil {
			return err
		}
		local := identitypkg.NewLocalIdentity(client.BaseURL, challenge.Value.IdentityID, handle, name, encID, sigID, createdAt, keys)
		path, err := store.SaveIdentity(local)
		if err != nil {
			return err
		}
		request := api.CreateIdentityRequest{IdentityID: challenge.Value.IdentityID, Handle: handle, DisplayName: name, ChallengeID: challenge.Value.ID, Challenge: challenge.Value.Challenge, EncryptionKey: enc, SigningKey: sign, EncryptionBindingSignature: base64.RawURLEncoding.EncodeToString(bindSig), PossessionSignature: base64.RawURLEncoding.EncodeToString(posSig)}
		pending.CreateIdempotencyKey = newIdempotencyKey()
		pending.KeyPath = path
		pending.Request = &request
		if _, err := store.SavePending("identity-create", handle, pending); err != nil {
			return fmt.Errorf("cannot persist recoverable identity publication; generated keys remain at %s: %w", path, err)
		}
		return a.publishPendingIdentityCreate(cmd.Context(), client, store, pending)
	}}
	cmd.Flags().StringVar(&name, "name", "", "display name")
	cmd.Flags().StringVar(&handle, "handle", "", "account-local lowercase slug")
	return cmd
}

func (a *app) publishPendingIdentityCreate(ctx context.Context, client *api.Client, store identitypkg.Store, pending pendingIdentityCreate) error {
	if pending.Request == nil || pending.CreateIdempotencyKey == "" || pending.KeyPath == "" || pending.Request.IdentityID == "" || pending.Request.Handle != pending.Handle || pending.Request.DisplayName != pending.DisplayName {
		return errors.New("stored pending identity publication is invalid")
	}
	local, err := store.LoadIdentity(pending.Request.IdentityID)
	if err != nil {
		return fmt.Errorf("pending identity private keys are unavailable: %w", err)
	}
	if err := ensureLocalIdentityServer(local, client.BaseURL); err != nil {
		return err
	}
	if local.Handle != pending.Handle || local.EncryptionKeyID != pending.Request.EncryptionKey.ID || local.SigningKeyID != pending.Request.SigningKey.ID || local.EncryptionPublicKey != pending.Request.EncryptionKey.PublicKey || local.SigningPublicKey != pending.Request.SigningKey.PublicKey {
		return errors.New("pending identity publication does not match its local private-key record")
	}
	res, err := client.CreateIdentity(ctx, *pending.Request, pending.CreateIdempotencyKey)
	if err != nil {
		return fmt.Errorf("identity publication response was not recovered; generated keys and exact replay state were retained at %s: %w", pending.KeyPath, err)
	}
	identity := res.Value.Identity
	if identity.ID != local.IdentityID {
		return errors.New("created identity response ID does not match reserved identity")
	}
	if err := store.RemovePending("identity-create", pending.Handle); err != nil {
		return fmt.Errorf("identity created but pending replay state could not be removed: %w", err)
	}
	if a.jsonOut {
		return a.printJSONValue(map[string]any{"identity": identity, "key_file": pending.KeyPath})
	}
	fmt.Fprintf(a.stdout, "created %s (%s)\nfingerprint %s\nkeys %s (0600)\n", identity.ID, pending.Handle, identitypkg.FormatFingerprint(identity.Fingerprint), pending.KeyPath)
	fmt.Fprintln(a.stderr, "warning: back up this file securely; lost private keys cannot decrypt unread deliveries")
	return nil
}

func (a *app) identityListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List account identities", Args: noArgs, RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.client()
		if err != nil {
			return err
		}
		res, err := client.ListIdentities(cmd.Context())
		if err != nil {
			return err
		}
		if a.jsonOut {
			a.printJSON(res.Raw)
			return nil
		}
		for _, id := range res.Value.Identities {
			fmt.Fprintf(a.stdout, "%s\t%s\t%s\t%s\n", id.ID, id.Handle, id.State, identitypkg.FormatFingerprint(id.Fingerprint))
		}
		return nil
	}}
}
func (a *app) identityShowCmd() *cobra.Command {
	return &cobra.Command{Use: "show <identity>", Short: "Show an owned identity and key history", Args: exactArgs(1, "identity ID"), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.client()
		if err != nil {
			return err
		}
		res, err := client.GetIdentity(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if a.jsonOut {
			a.printJSON(res.Raw)
			return nil
		}
		id := res.Value.Identity
		fmt.Fprintf(a.stdout, "%s (%s)\nstate %s\nfingerprint %s\n", id.DisplayName, id.Handle, id.State, identitypkg.FormatFingerprint(id.Fingerprint))
		return nil
	}}
}
func (a *app) identityDisableCmd() *cobra.Command {
	return &cobra.Command{Use: "disable <identity>", Short: "Disable public lookup and new deliveries", Args: exactArgs(1, "identity ID"), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.client()
		if err != nil {
			return err
		}
		res, err := client.PatchIdentity(cmd.Context(), args[0], map[string]any{"disabled": true})
		if err != nil {
			return err
		}
		if a.jsonOut {
			a.printJSON(res.Raw)
		} else {
			fmt.Fprintf(a.stdout, "disabled %s\n", res.Value.Identity.ID)
		}
		return nil
	}}
}

func (a *app) identityRotateCmd() *cobra.Command {
	var purpose string
	cmd := &cobra.Command{Use: "rotate <identity>", Short: "Rotate an identity key with predecessor authorization", Args: exactArgs(1, "local identity ID or handle"), RunE: func(cmd *cobra.Command, args []string) error {
		if purpose != "encryption" && purpose != "signing" {
			return usagef("--purpose must be encryption or signing")
		}
		client, err := a.client()
		if err != nil {
			return err
		}
		store, err := a.identityStore()
		if err != nil {
			return err
		}
		local, err := store.LoadIdentity(args[0])
		if err != nil {
			return err
		}
		if err := ensureLocalIdentityServer(local, client.BaseURL); err != nil {
			return err
		}
		var pending pendingIdentityRotation
		if pendingErr := store.LoadPending("rotation", local.IdentityID, &pending); pendingErr == nil {
			if pending.Version != 1 || pending.ServerURL != client.BaseURL || pending.IdentityID != local.IdentityID || pending.SuccessorKeyID == "" || pending.StagedPath == "" || pending.IdempotencyKey == "" || pending.Request.Key.ID != pending.SuccessorKeyID || pending.Request.Purpose != pending.Purpose {
				return errors.New("stored pending rotation record is invalid")
			}
			if pending.Purpose != purpose {
				return usagef("a %s rotation is pending; rerun with --purpose %s", pending.Purpose, pending.Purpose)
			}
			return a.publishPendingRotation(cmd.Context(), client, store, pending)
		} else if !errors.Is(pendingErr, os.ErrNotExist) {
			return pendingErr
		}
		challenge, err := client.CreateIdentityChallenge(cmd.Context(), "identity.rotate", local.IdentityID, newIdempotencyKey())
		if err != nil {
			return err
		}
		keys, err := identitypkg.GenerateKeyPair()
		if err != nil {
			return err
		}
		createdAt := challenge.Value.IssuedAt
		successor := newULID(createdAt)
		var key api.SuccessorIdentityKey
		var predecessor string
		if purpose == "encryption" {
			key = api.SuccessorIdentityKey{ID: successor, Algorithm: identitypkg.EncryptionAlgorithm, PublicKey: identitypkg.PublicKeyString(keys.EncryptionPublic), CreatedAt: createdAt, NotAfter: nil}
			predecessor = local.EncryptionKeyID
		} else {
			key = api.SuccessorIdentityKey{ID: successor, Algorithm: identitypkg.SigningAlgorithm, PublicKey: identitypkg.PublicKeyString(keys.SigningPublic), CreatedAt: createdAt, NotAfter: nil}
			predecessor = local.SigningKeyID
		}
		statement := struct {
			Algorithm        string `json:"algorithm"`
			IdentityID       string `json:"identity_id"`
			NotAfter         *int64 `json:"not_after"`
			PredecessorKeyID string `json:"predecessor_key_id"`
			PublicKey        string `json:"public_key"`
			Purpose          string `json:"purpose"`
			SuccessorKeyID   string `json:"successor_key_id"`
			ValidFrom        int64  `json:"valid_from"`
		}{key.Algorithm, local.IdentityID, nil, predecessor, key.PublicKey, purpose, successor, createdAt}
		oldSigning, err := local.SigningPrivate()
		if err != nil {
			return err
		}
		canonical, err := identitypkg.CanonicalJSON(statement)
		if err != nil {
			return err
		}
		sig, err := identitypkg.SignCanonical(oldSigning, identitypkg.SuccessorSignatureDomain, canonical)
		if err != nil {
			return err
		}
		if purpose == "encryption" {
			local.EncryptionKeyID = successor
			local.EncryptionPrivateKey = base64.RawURLEncoding.EncodeToString(keys.EncryptionPrivate)
			local.EncryptionPublicKey = key.PublicKey
			local.EncryptionKeys = append(local.EncryptionKeys, identitypkg.LocalPrivateKey{KeyID: successor, PrivateKey: local.EncryptionPrivateKey, PublicKey: key.PublicKey, CreatedAt: createdAt})
		} else {
			local.SigningKeyID = successor
			local.SigningPrivateKey = base64.RawURLEncoding.EncodeToString(keys.SigningPrivate)
			local.SigningPublicKey = key.PublicKey
			local.SigningKeys = append(local.SigningKeys, identitypkg.LocalPrivateKey{KeyID: successor, PrivateKey: local.SigningPrivateKey, PublicKey: key.PublicKey, CreatedAt: createdAt})
		}
		staged, err := store.StageIdentity(local, successor)
		if err != nil {
			return fmt.Errorf("cannot safely stage new private key: %w", err)
		}
		pending = pendingIdentityRotation{
			Version: 1, ServerURL: client.BaseURL, IdentityID: local.IdentityID, SuccessorKeyID: successor, Purpose: purpose,
			StagedPath: staged, IdempotencyKey: newIdempotencyKey(),
			Request: api.RotateIdentityKeyRequest{ChallengeID: challenge.Value.ID, Challenge: challenge.Value.Challenge, Purpose: purpose, Key: key, PredecessorKeyID: predecessor, RotationSignature: base64.RawURLEncoding.EncodeToString(sig)},
		}
		if _, err := store.SavePending("rotation", local.IdentityID, pending); err != nil {
			return fmt.Errorf("cannot persist recoverable rotation request; staged private key remains at %s: %w", staged, err)
		}
		return a.publishPendingRotation(cmd.Context(), client, store, pending)
	}}
	cmd.Flags().StringVar(&purpose, "purpose", "", "key purpose: encryption or signing")
	return cmd
}

func (a *app) publishPendingRotation(ctx context.Context, client *api.Client, store identitypkg.Store, pending pendingIdentityRotation) error {
	res, err := client.RotateIdentityKey(ctx, pending.IdentityID, pending.Request, pending.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("rotation response was not recovered; pending private key and exact replay request remain at %s: %w", pending.StagedPath, err)
	}
	if err := store.PromoteStagedIdentity(pending.StagedPath, pending.IdentityID); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("key rotated remotely; staged private key remains at %s: %w", pending.StagedPath, err)
		}
		local, loadErr := store.LoadIdentity(pending.IdentityID)
		if loadErr != nil || pending.Purpose == "encryption" && local.EncryptionKeyID != pending.SuccessorKeyID || pending.Purpose == "signing" && local.SigningKeyID != pending.SuccessorKeyID {
			return fmt.Errorf("key rotated remotely but the staged private key is unavailable: %w", err)
		}
	}
	if err := store.RemovePending("rotation", pending.IdentityID); err != nil {
		return fmt.Errorf("rotation completed but pending replay state could not be removed: %w", err)
	}
	if a.jsonOut {
		a.printJSON(res.Raw)
	} else {
		fmt.Fprintf(a.stdout, "rotated %s key to %s\n", pending.Purpose, pending.SuccessorKeyID)
		fmt.Fprintln(a.stderr, "warning: keep old encryption keys until every delivery addressed to them expires")
	}
	return nil
}

func (a *app) identityRevokeCmd() *cobra.Command {
	return &cobra.Command{Use: "revoke <identity> <key-id>", Short: "Revoke a public identity key for new use", Args: exactArgs(2, "identity ID and key ID"), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.client()
		if err != nil {
			return err
		}
		if err := client.RevokeIdentityKey(cmd.Context(), args[0], args[1], newIdempotencyKey()); err != nil {
			return err
		}
		if !a.jsonOut {
			fmt.Fprintf(a.stdout, "revoked key %s\n", args[1])
		}
		return nil
	}}
}

func validHandle(v string) bool {
	if len(v) < 1 || len(v) > 63 || v[0] == '-' || v[len(v)-1] == '-' {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func ensureLocalIdentityServer(local identitypkg.LocalIdentity, configured string) error {
	if local.ServerURL == "" {
		return errors.New("local identity predates server binding and cannot be used safely")
	}
	canonical, err := identitypkg.CanonicalServerURL(configured)
	if err != nil {
		return err
	}
	if local.ServerURL != canonical {
		return usagef("identity belongs to %s, not configured server %s", local.ServerURL, canonical)
	}
	return nil
}

func newULID(now int64) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	millis := uint64(now) * 1000
	var out [26]byte
	for i := 9; i >= 0; i-- {
		out[i] = alphabet[millis&31]
		millis >>= 5
	}
	var acc uint32
	bits, j := 0, 10
	for _, b := range raw[6:] {
		acc = (acc << 8) | uint32(b)
		bits += 8
		for bits >= 5 && j < 26 {
			bits -= 5
			out[j] = alphabet[(acc>>bits)&31]
			j++
		}
	}
	for j < 26 {
		out[j] = alphabet[0]
		j++
	}
	return string(out[:])
}
