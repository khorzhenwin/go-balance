package types

import (
	"net/url"
	"sync"
)

type Server struct {
	URL       *url.URL
	IsHealthy bool
	Mutex     sync.Mutex
}
