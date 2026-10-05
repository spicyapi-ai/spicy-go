module github.com/spicyapi-ai/spicy-go

go 1.23

// v0.1.0 shipped source comments that were not in English. The module proxy caches
// versions immutably and sum.golang.org has its checksum on record, so the tag cannot
// be corrected in place - moving it would only make `go get v0.1.0` fail checksum
// verification, which is worse than the comments. Retracting it is the mechanism Go
// provides: `go list -m -versions` stops offering it and `go get` will not select it.
// v0.1.1 is identical apart from the comments.
retract v0.1.0
