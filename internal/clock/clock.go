// Package clock provides the current time. Tests replace Now to freeze it.
package clock

import "time"

// Now returns the current time.
var Now = time.Now
