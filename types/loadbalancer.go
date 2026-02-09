package types

import "sync"

type LoadBalancer struct {
	Current int
	Mutex   sync.Mutex
}
