module teslalog

go 1.24.7

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/ncruces/go-sqlite3 v0.20.0
)

require (
	github.com/ncruces/julianday v1.0.0 // indirect
	github.com/tetratelabs/wazero v1.8.1 // indirect
	golang.org/x/sys v0.26.0 // indirect
)

replace golang.org/x/sys => github.com/golang/sys v0.26.0
