package memory

import "time"

// SetTimeout replaces ReviewTimeout for one review.
func (o *ReviewOptions) SetTimeout(timeout time.Duration) { o.timeout = timeout }
