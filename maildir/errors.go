package maildir

import "errors"

var ErrNotExist = errors.New("maildir: not exist")

func IsNotExist(err error) bool {
	return errors.Is(err, ErrNotExist)
}
