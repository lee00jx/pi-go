module example/hello

go 1.24.0

require github.com/lee00jx/pi-go v0.0.0

// Local development: point at the library checkout. When publishing, drop
// the replace and require a real version.
replace github.com/lee00jx/pi-go => ../..
