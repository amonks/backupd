package snitch

import "net/http"

func OK(id string) error {
	return New(id).OK()
}

func Error(id string, err error) error {
	return New(id).Error(err)
}

type Snitcher struct {
	id string
}

func New(id string) *Snitcher {
	return &Snitcher{id}
}

func (sn *Snitcher) OK() error {
	if _, err := http.Get("https://nosnch.in/" + sn.id); err != nil {
		return err
	}
	return nil
}

func (sn *Snitcher) Error(err error) error {
	if err != nil {
		return nil
	}
	return sn.OK()
}
