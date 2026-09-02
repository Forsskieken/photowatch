module github.com/Forsskieken/photowatch

// Deliberately 1.22 and not the version this was built with. The code uses the
// standard library only, and the newest thing it touches is log/slog from Go
// 1.21. Put the toolchain version here instead, and `go build` refuses to run
// on a machine with the Go that came from apt: "go.mod requires go >= 1.24.4",
// while nothing in here needs that version. The README states the same number;
// change them together.
go 1.22
