package main

import (
	"errors"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

// credentialSourceAdapter adapts *custos.Vault to surface.CredentialSource
// (CREDHANDLE-SPEC R4 / D5). The daemon holds the mutation key after unlock,
// so mutations pass an empty passphrase (CUSTOS-SPEC §C3, as ctl handlers do).
type credentialSourceAdapter struct {
	vault *custos.Vault
}

var _ surface.CredentialSource = (*credentialSourceAdapter)(nil)

const credActorWeb = "web"

func infoOf(v custos.HandleView) surface.CredentialInfo {
	out := surface.CredentialInfo{
		Handle: v.Handle, Name: v.Name, Kind: v.Kind, Last4: v.Last4, Created: v.Created,
	}
	if v.Expires != "" {
		e := v.Expires
		out.Expires = &e
	}
	if v.Rate != nil {
		out.Rate = &surface.CredentialRate{N: v.Rate.N, Window: v.Rate.Window}
	}
	return out
}

// mapCredErr translates custos errors to surface sentinels. Error text is
// dropped on purpose; only classes cross the boundary.
func mapCredErr(err error) error {
	var fe *custos.FieldError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, custos.ErrCustosLocked):
		return surface.ErrLocked
	case errors.As(err, &fe):
		return &surface.CredentialFieldError{Field: fe.Field}
	case errors.Is(err, custos.ErrHandleNotFound):
		return surface.ErrCredentialNotFound
	case errors.Is(err, custos.ErrCredentialExists):
		return surface.ErrCredentialExists
	case errors.Is(err, custos.ErrFull):
		return surface.ErrCredentialFull
	default:
		return errors.New("credential operation failed")
	}
}

func (a *credentialSourceAdapter) List() ([]surface.CredentialInfo, error) {
	views, err := a.vault.ListHandleViews()
	if err != nil {
		return nil, mapCredErr(err)
	}
	out := make([]surface.CredentialInfo, len(views))
	for i, v := range views {
		out[i] = infoOf(v)
	}
	return out, nil
}

func (a *credentialSourceAdapter) Create(req surface.CredentialCreate) (surface.CredentialInfo, error) {
	var rate *custos.HandleRate
	if req.Rate != nil {
		rate = &custos.HandleRate{N: req.Rate.N, Window: req.Rate.Window}
	}
	rec, err := a.vault.CreateCredentialHandle("", req.Name, req.Secret, req.Scope,
		time.Duration(req.TTLSeconds)*time.Second, rate, credActorWeb)
	if err != nil {
		return surface.CredentialInfo{}, mapCredErr(err)
	}
	return infoOf(custos.ViewOf(rec, "api_key")), nil
}

// Revoke removes the handle only; the named credential stays (matches
// `custos handle revoke`).
func (a *credentialSourceAdapter) Revoke(handle string) error {
	return mapCredErr(a.vault.RevokeHandle("", handle, credActorWeb))
}
