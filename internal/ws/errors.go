package ws

import "errors"

var (
	ErrConnectionClosed = errors.New("connection is closed")
	ErrQueueFull        = errors.New("outbound queue is full")
)