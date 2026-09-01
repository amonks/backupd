package snitch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

func OK(ctx context.Context, id string) error {
	return New(id).OK(ctx)
}

func Error(ctx context.Context, id string, err error) error {
	return New(id).Error(ctx, err)
}

type Snitcher struct {
	id string
}

func New(id string) *Snitcher {
	return &Snitcher{id}
}

func (sn *Snitcher) OK(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://nosnch.in/"+sn.id, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_, drainErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	var statusErr error
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		statusErr = fmt.Errorf("snitch check-in: %s", resp.Status)
	}
	return errors.Join(statusErr, drainErr, closeErr)
}

func (sn *Snitcher) Error(ctx context.Context, err error) error {
	if err != nil {
		return nil
	}
	return sn.OK(ctx)
}
