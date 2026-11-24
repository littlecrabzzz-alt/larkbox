package protocol

import "regexp"

var assignedMention = regexp.MustCompile(`@_user_[0-9]+`)

// HasAssignedMention reports whether decoded content already uses a mention key.
func HasAssignedMention(decodedJSON []byte) bool {
	return assignedMention.Match(decodedJSON)
}
