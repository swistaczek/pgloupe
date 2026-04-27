package main

import (
	"bytes"
	"strconv"
)

// parseCommandTag extracts the trailing row count from a CommandComplete tag.
// "SELECT 5", "UPDATE 3", "DELETE 12", "INSERT 0 1" all match.
// "BEGIN", "COMMIT", "SET", "CREATE TABLE" return ok=false.
//
// Per PG 17 §53.7, the row count is always the last numeric field — including
// the INSERT 3-field "INSERT oid rows" form.
func parseCommandTag(tag []byte) (rows int64, ok bool) {
	parts := bytes.Fields(tag)
	if len(parts) < 2 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(parts[len(parts)-1]), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
