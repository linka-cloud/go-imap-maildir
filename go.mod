module github.com/foxcpp/go-imap-maildir

go 1.22

require (
	github.com/emersion/go-imap v1.2.2-0.20220928192137-6fac715be9cf
	github.com/emersion/go-maildir v0.6.0
	github.com/emersion/go-message v0.18.2
	github.com/foxcpp/go-imap-backend-tests v0.0.0-20220105184719-e80aa29a5e16
	github.com/foxcpp/go-imap-mess v0.0.0-20230108134257-b7ec3a649613
)

require (
	github.com/emersion/go-sasl v0.0.0-20200509203442-7bfe0ed36a21 // indirect
	github.com/google/go-cmp v0.6.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	golang.org/x/text v0.14.0 // indirect
	gotest.tools v2.2.0+incompatible // indirect
)

replace github.com/emersion/go-imap => github.com/foxcpp/go-imap v1.0.0-beta.1.0.20220623182312-df940c324887
