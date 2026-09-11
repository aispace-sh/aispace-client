package cmd

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/aispace-sh/aispace-client/internal/api"
	identitypkg "github.com/aispace-sh/aispace-client/internal/identity"
)

func (a *app) recipientCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "recipient", Short: "Manage locally trusted delivery recipients", Args: noArgs}
	cmd.AddCommand(a.recipientAddCmd(), a.recipientVerifyCmd(), a.recipientListCmd(), a.recipientRemoveCmd())
	return cmd
}

func activeIdentityKeys(id api.Identity) (*api.IdentityKey, *api.IdentityKey, error) {
	enc, sign := id.EncryptionKey, id.SigningKey
	if enc == nil {
		enc = id.ActiveEncryptionKey
	}
	if sign == nil {
		sign = id.ActiveSigningKey
	}
	if enc == nil || sign == nil {
		return nil, nil, errors.New("public identity omitted active encryption or signing key")
	}
	if enc.Algorithm != identitypkg.EncryptionAlgorithm || sign.Algorithm != identitypkg.SigningAlgorithm {
		return nil, nil, errors.New("public identity uses unsupported key algorithms")
	}
	if enc.KeyID == "" || sign.KeyID == "" || enc.IdentityID != id.ID || sign.IdentityID != id.ID || enc.Purpose != "encryption" || sign.Purpose != "signing" {
		return nil, nil, errors.New("public identity contains invalid active key bindings")
	}
	return enc, sign, nil
}

func recipientFromPublic(serverURL, alias string, id api.Identity, trust string, now int64) (identitypkg.Recipient, error) {
	enc, sign, err := activeIdentityKeys(id)
	if err != nil {
		return identitypkg.Recipient{}, err
	}
	encPub, err := identitypkg.ParsePublicKey(enc.PublicKey, 32)
	if err != nil {
		return identitypkg.Recipient{}, err
	}
	signPub, err := identitypkg.ParsePublicKey(sign.PublicKey, ed25519.PublicKeySize)
	if err != nil {
		return identitypkg.Recipient{}, err
	}
	fp := identitypkg.Fingerprint(id.ID, enc.KeyID, encPub, sign.KeyID, signPub)
	supplied, err := identitypkg.ParseFingerprint(id.Fingerprint)
	if err != nil {
		return identitypkg.Recipient{}, errors.New("service returned an invalid identity fingerprint")
	}
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(fp)) != 1 {
		return identitypkg.Recipient{}, errors.New("service fingerprint does not match its public key record")
	}
	return identitypkg.Recipient{Alias: alias, ServerURL: serverURL, IdentityID: id.ID, Handle: id.Handle, DisplayName: id.DisplayName, EncryptionKeyID: enc.KeyID, EncryptionPublicKey: enc.PublicKey, SigningKeyID: sign.KeyID, SigningPublicKey: sign.PublicKey, Fingerprint: fp, TrustState: trust, CreatedAt: now, UpdatedAt: now}, nil
}

func (a *app) recipientAddCmd() *cobra.Command {
	var alias string
	cmd := &cobra.Command{Use: "add <invitation-url>", Short: "Import an exact identity invitation as unverified", Args: exactArgs(1, "invitation URL"), RunE: func(cmd *cobra.Command, args []string) error {
		serverURL, id, want, err := identitypkg.ParseInvitation(args[0])
		if err != nil {
			return usagef("invalid invitation: %v", err)
		}
		client, err := a.client()
		if err != nil {
			return err
		}
		if serverURL != client.BaseURL {
			return usagef("invitation belongs to %s, not configured server %s", serverURL, client.BaseURL)
		}
		res, err := client.GetPublicIdentity(cmd.Context(), id)
		if err != nil {
			return err
		}
		if res.Value.ID != id {
			return errors.New("public identity response ID does not match invitation")
		}
		if alias == "" {
			alias = res.Value.Handle
		}
		if !validHandle(alias) {
			return usagef("--alias must be a lowercase slug")
		}
		recipient, err := recipientFromPublic(serverURL, alias, res.Value, "unverified", time.Now().Unix())
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(recipient.Fingerprint), []byte(want)) != 1 {
			return &codedError{code: "fingerprint_mismatch", err: errors.New("invitation fingerprint does not match the published identity keys"), exit: ExitGeneric}
		}
		store, err := a.identityStore()
		if err != nil {
			return err
		}
		if previous, e := store.Recipient(id); e == nil && (previous.TrustState == "pinned" || previous.TrustState == "rotated") {
			if previous.Fingerprint != recipient.Fingerprint {
				previous.TrustState = "changed"
				previous.UpdatedAt = time.Now().Unix()
				_ = store.PutRecipient(previous)
				return &codedError{code: "recipient_key_changed", err: errors.New("published recipient keys differ from the pinned fingerprint; explicit re-verification is required"), exit: ExitGeneric}
			}
			recipient.TrustState = previous.TrustState
			recipient.CreatedAt = previous.CreatedAt
		}
		if _, err := client.PutRecipientPin(cmd.Context(), api.RecipientPin{IdentityID: id, KeyID: recipient.EncryptionKeyID, LocalAlias: alias, Fingerprint: recipient.Fingerprint, TrustState: recipient.TrustState}, newIdempotencyKey()); err != nil {
			return err
		}
		if err := store.PutRecipient(recipient); err != nil {
			return err
		}
		if a.jsonOut {
			return a.printJSONValue(recipient)
		}
		fmt.Fprintf(a.stdout, "added %s (%s)\nfingerprint %s\ntrust %s\n", alias, id, identitypkg.FormatFingerprint(recipient.Fingerprint), recipient.TrustState)
		if recipient.TrustState == "unverified" {
			fmt.Fprintf(a.stdout, "run `aispace recipient verify %s --fingerprint ...`\n", alias)
		}
		return nil
	}}
	cmd.Flags().StringVar(&alias, "alias", "", "local recipient alias (default: public handle)")
	return cmd
}

func (a *app) recipientVerifyCmd() *cobra.Command {
	var fingerprint string
	cmd := &cobra.Command{Use: "verify <recipient>", Short: "Pin an exact recipient fingerprint", Args: exactArgs(1, "recipient alias or identity ID"), RunE: func(cmd *cobra.Command, args []string) error {
		want, err := identitypkg.ParseFingerprint(fingerprint)
		if err != nil {
			return usagef("--fingerprint: %v", err)
		}
		store, err := a.identityStore()
		if err != nil {
			return err
		}
		recipient, err := store.Recipient(args[0])
		if err != nil {
			return err
		}
		client, err := a.client()
		if err != nil {
			return err
		}
		if err := ensureRecipientServer(recipient, client.BaseURL); err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(recipient.Fingerprint)) != 1 {
			return &codedError{code: "fingerprint_mismatch", err: errors.New("fingerprint does not match the imported public keys"), exit: ExitGeneric}
		}
		if _, err := client.PutRecipientPin(cmd.Context(), api.RecipientPin{IdentityID: recipient.IdentityID, KeyID: recipient.EncryptionKeyID, LocalAlias: recipient.Alias, Fingerprint: recipient.Fingerprint, TrustState: "pinned"}, newIdempotencyKey()); err != nil {
			return err
		}
		recipient.TrustState = "pinned"
		recipient.UpdatedAt = time.Now().Unix()
		if err := store.PutRecipient(recipient); err != nil {
			return err
		}
		if a.jsonOut {
			return a.printJSONValue(recipient)
		}
		fmt.Fprintf(a.stdout, "pinned %s\nfingerprint %s\n", recipient.Alias, identitypkg.FormatFingerprint(recipient.Fingerprint))
		return nil
	}}
	cmd.Flags().StringVar(&fingerprint, "fingerprint", "", "expected full 32-byte fingerprint")
	return cmd
}

func (a *app) recipientListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List locally known recipients", Args: noArgs, RunE: func(cmd *cobra.Command, args []string) error {
		store, err := a.identityStore()
		if err != nil {
			return err
		}
		recipients, err := store.ListRecipients()
		if err != nil {
			return err
		}
		if a.jsonOut {
			return a.printJSONValue(map[string]any{"recipients": recipients})
		}
		for _, r := range recipients {
			fmt.Fprintf(a.stdout, "%s\t%s\t%s\t%s\n", r.Alias, r.IdentityID, r.TrustState, identitypkg.FormatFingerprint(r.Fingerprint))
		}
		return nil
	}}
}
func (a *app) recipientRemoveCmd() *cobra.Command {
	return &cobra.Command{Use: "remove <recipient>", Aliases: []string{"rm"}, Short: "Remove a recipient pin from the service and local trust store", Args: exactArgs(1, "recipient alias or identity ID"), RunE: func(cmd *cobra.Command, args []string) error {
		store, err := a.identityStore()
		if err != nil {
			return err
		}
		recipient, err := store.Recipient(args[0])
		if err != nil {
			return err
		}
		client, err := a.client()
		if err != nil {
			return err
		}
		if err := ensureRecipientServer(recipient, client.BaseURL); err != nil {
			return err
		}
		if err := client.DeleteRecipientPin(cmd.Context(), recipient.IdentityID); err != nil {
			return err
		}
		if err := store.RemoveRecipient(recipient.IdentityID); err != nil {
			return err
		}
		if !a.jsonOut {
			fmt.Fprintf(a.stdout, "removed %s\n", recipient.Alias)
		}
		return nil
	}}
}

func (a *app) resolveRecipient(ctx context.Context, ref string, trustOnFirstUse bool, expected string) (identitypkg.Recipient, error) {
	store, err := a.identityStore()
	if err != nil {
		return identitypkg.Recipient{}, err
	}
	saved, err := store.Recipient(ref)
	if err != nil {
		return identitypkg.Recipient{}, err
	}
	if saved.TrustState == "unverified" && !trustOnFirstUse && expected == "" {
		return identitypkg.Recipient{}, &codedError{code: "recipient_unverified", err: errors.New("recipient is unverified; supply --recipient-fingerprint or explicitly use --trust-on-first-use"), exit: ExitUsage}
	}
	client, err := a.client()
	if err != nil {
		return identitypkg.Recipient{}, err
	}
	if err := ensureRecipientServer(saved, client.BaseURL); err != nil {
		return identitypkg.Recipient{}, err
	}
	res, err := client.GetPublicIdentity(ctx, saved.IdentityID)
	if err != nil {
		return identitypkg.Recipient{}, err
	}
	current, err := recipientFromPublic(saved.ServerURL, saved.Alias, res.Value, saved.TrustState, time.Now().Unix())
	if err != nil {
		return identitypkg.Recipient{}, err
	}
	if current.Fingerprint != saved.Fingerprint {
		validRotation, rotationErr := validateRecipientRotation(ctx, client, saved, current, res.Value)
		if rotationErr != nil {
			return identitypkg.Recipient{}, fmt.Errorf("validate recipient key history: %w", rotationErr)
		}
		if validRotation {
			current.TrustState = "rotated"
			current.CreatedAt = saved.CreatedAt
			current.UpdatedAt = time.Now().Unix()
			if _, err := client.PutRecipientPin(ctx, api.RecipientPin{IdentityID: current.IdentityID, KeyID: current.EncryptionKeyID, LocalAlias: current.Alias, Fingerprint: current.Fingerprint, TrustState: "rotated"}, newIdempotencyKey()); err != nil {
				return identitypkg.Recipient{}, err
			}
			if err := store.PutRecipient(current); err != nil {
				return identitypkg.Recipient{}, err
			}
			return current, nil
		}
		saved.TrustState = "changed"
		saved.UpdatedAt = time.Now().Unix()
		_ = store.PutRecipient(saved)
		return identitypkg.Recipient{}, &codedError{code: "recipient_key_changed", err: errors.New("recipient public keys changed; sending is blocked until explicitly verified"), exit: ExitGeneric}
	}
	if expected != "" {
		want, e := identitypkg.ParseFingerprint(expected)
		if e != nil {
			return identitypkg.Recipient{}, usagef("--recipient-fingerprint: %v", e)
		}
		if want != saved.Fingerprint {
			return identitypkg.Recipient{}, &codedError{code: "fingerprint_mismatch", err: errors.New("expected recipient fingerprint does not match"), exit: ExitGeneric}
		}
		if _, err := client.PutRecipientPin(ctx, api.RecipientPin{IdentityID: saved.IdentityID, KeyID: saved.EncryptionKeyID, LocalAlias: saved.Alias, Fingerprint: saved.Fingerprint, TrustState: "pinned"}, newIdempotencyKey()); err != nil {
			return identitypkg.Recipient{}, err
		}
		saved.TrustState = "pinned"
		saved.UpdatedAt = time.Now().Unix()
		if err := store.PutRecipient(saved); err != nil {
			return identitypkg.Recipient{}, err
		}
	}
	if saved.TrustState == "unverified" {
		if !trustOnFirstUse {
			return identitypkg.Recipient{}, &codedError{code: "recipient_unverified", err: errors.New("recipient is unverified; supply --recipient-fingerprint or explicitly use --trust-on-first-use"), exit: ExitUsage}
		}
		if _, err := client.PutRecipientPin(ctx, api.RecipientPin{IdentityID: saved.IdentityID, KeyID: saved.EncryptionKeyID, LocalAlias: saved.Alias, Fingerprint: saved.Fingerprint, TrustState: "pinned"}, newIdempotencyKey()); err != nil {
			return identitypkg.Recipient{}, err
		}
		saved.TrustState = "pinned"
		saved.UpdatedAt = time.Now().Unix()
		if err := store.PutRecipient(saved); err != nil {
			return identitypkg.Recipient{}, err
		}
		fmt.Fprintf(a.stderr, "warning: trusted %s on first use as %s\n", saved.Alias, identitypkg.FormatFingerprint(saved.Fingerprint))
	}
	if saved.TrustState != "pinned" && saved.TrustState != "rotated" {
		return identitypkg.Recipient{}, &codedError{code: "recipient_not_trusted", err: fmt.Errorf("recipient trust state is %s", saved.TrustState), exit: ExitGeneric}
	}
	return saved, nil
}

func ensureRecipientServer(recipient identitypkg.Recipient, configured string) error {
	if recipient.ServerURL == "" {
		return errors.New("recipient trust entry predates server binding; re-import its invitation")
	}
	canonical, err := identitypkg.CanonicalServerURL(configured)
	if err != nil {
		return err
	}
	if recipient.ServerURL != canonical {
		return usagef("recipient belongs to %s, not configured server %s", recipient.ServerURL, canonical)
	}
	return nil
}

const maxRecipientRotationDepth = 32

func validateRecipientRotation(ctx context.Context, client *api.Client, old, current identitypkg.Recipient, public api.Identity) (bool, error) {
	return validRecipientRotation(old, current, public, func(keyID string) (api.IdentityKey, error) {
		res, err := client.GetPublicIdentityKey(ctx, old.IdentityID, keyID)
		return res.Value.Key, err
	})
}

func validRecipientRotation(old, current identitypkg.Recipient, public api.Identity, fetch func(string) (api.IdentityKey, error)) (bool, error) {
	enc, sign, err := activeIdentityKeys(public)
	if err != nil {
		return false, nil
	}
	if old.IdentityID != current.IdentityID || current.IdentityID != public.ID {
		return false, nil
	}
	if !samePublicKeyBinding(current.EncryptionKeyID, current.EncryptionPublicKey, enc.KeyID, enc.PublicKey, 32) || !samePublicKeyBinding(current.SigningKeyID, current.SigningPublicKey, sign.KeyID, sign.PublicKey, ed25519.PublicKeySize) {
		return false, nil
	}
	signingChain, ok, err := walkRecipientKeyChain(old.IdentityID, "signing", identitypkg.SigningAlgorithm, sign, old.SigningKeyID, old.SigningPublicKey, ed25519.PublicKeySize, fetch)
	if err != nil || !ok {
		return false, err
	}
	for i := 0; i+1 < len(signingChain); i++ {
		if !verifyRecipientSuccessor(signingChain[i], signingChain[i+1], signingChain[i+1]) {
			return false, nil
		}
	}
	encryptionChain, ok, err := walkRecipientKeyChain(old.IdentityID, "encryption", identitypkg.EncryptionAlgorithm, enc, old.EncryptionKeyID, old.EncryptionPublicKey, 32, fetch)
	if err != nil || !ok {
		return false, err
	}
	for i := 0; i+1 < len(encryptionChain); i++ {
		verified := false
		for _, authorizer := range signingChain {
			if verifyRecipientSuccessor(encryptionChain[i], encryptionChain[i+1], authorizer) {
				verified = true
				break
			}
		}
		if !verified {
			return false, nil
		}
	}
	changed := old.EncryptionKeyID != current.EncryptionKeyID || old.SigningKeyID != current.SigningKeyID
	return changed, nil
}

func walkRecipientKeyChain(identityID, purpose, algorithm string, active *api.IdentityKey, pinnedID, pinnedPublic string, size int, fetch func(string) (api.IdentityKey, error)) ([]api.IdentityKey, bool, error) {
	chain := make([]api.IdentityKey, 0, 4)
	current := *active
	seen := make(map[string]struct{})
	for len(chain) < maxRecipientRotationDepth {
		if current.IdentityID != identityID || current.Purpose != purpose || current.Algorithm != algorithm || current.KeyID == "" {
			return nil, false, nil
		}
		if _, err := identitypkg.ParsePublicKey(current.PublicKey, size); err != nil {
			return nil, false, nil
		}
		if _, duplicate := seen[current.KeyID]; duplicate {
			return nil, false, nil
		}
		seen[current.KeyID] = struct{}{}
		chain = append(chain, current)
		if current.KeyID == pinnedID {
			if !samePublicKeyBinding(pinnedID, pinnedPublic, current.KeyID, current.PublicKey, size) {
				return nil, false, nil
			}
			return chain, true, nil
		}
		if current.PreviousKeyID == nil || *current.PreviousKeyID == "" || current.RotationSignature == "" {
			return nil, false, nil
		}
		predecessor, err := fetch(*current.PreviousKeyID)
		if err != nil {
			return nil, false, err
		}
		if predecessor.KeyID != *current.PreviousKeyID {
			return nil, false, nil
		}
		current = predecessor
	}
	return nil, false, nil
}

func verifyRecipientSuccessor(successor, predecessor, authorizer api.IdentityKey) bool {
	if successor.PreviousKeyID == nil || *successor.PreviousKeyID != predecessor.KeyID || successor.RotationSignature == "" || successor.CreatedAt < predecessor.CreatedAt || authorizer.Purpose != "signing" || authorizer.Algorithm != identitypkg.SigningAlgorithm || authorizer.CreatedAt > successor.CreatedAt {
		return false
	}
	if authorizer.NotAfter != nil && successor.CreatedAt > *authorizer.NotAfter || authorizer.RevokedAt != nil && successor.CreatedAt > *authorizer.RevokedAt {
		return false
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
	}{successor.Algorithm, successor.IdentityID, successor.NotAfter, predecessor.KeyID, successor.PublicKey, successor.Purpose, successor.KeyID, successor.CreatedAt}
	canonical, err := identitypkg.CanonicalJSON(statement)
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(successor.RotationSignature)
	if err != nil {
		return false
	}
	oldPub, err := identitypkg.ParsePublicKey(authorizer.PublicKey, ed25519.PublicKeySize)
	if err != nil {
		return false
	}
	return identitypkg.VerifyCanonical(ed25519.PublicKey(oldPub), identitypkg.SuccessorSignatureDomain, canonical, sig)
}

func samePublicKeyBinding(oldID, oldEncoded, currentID, currentEncoded string, size int) bool {
	if oldID != currentID {
		return false
	}
	oldKey, err := identitypkg.ParsePublicKey(oldEncoded, size)
	if err != nil {
		return false
	}
	currentKey, err := identitypkg.ParsePublicKey(currentEncoded, size)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(oldKey, currentKey) == 1
}
