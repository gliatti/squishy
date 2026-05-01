package connection

import "time"

func timeoutSeconds(n int) time.Duration {
	return time.Duration(n) * time.Second
}
