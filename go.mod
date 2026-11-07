module github.com/team-quaver/liino

go 1.27.3

require (
	github.com/bnema/purego-pipewire v0.1.6
	github.com/deploymenttheory/go-bindings-macosplatform v0.20.1
	github.com/deploymenttheory/go-bindings-win32 v0.5.0
	github.com/ebitengine/purego v0.11.1
	github.com/jfreymuth/oggvorbis v1.0.5
	github.com/team-quaver/ffgo v0.0.0
	github.com/thesyncim/gopus v0.2.3
	github.com/vcaesar/audio-go v0.10.0
)

require (
	github.com/bnema/purego v0.11.0-bnema.3 // indirect
	github.com/jfreymuth/vorbis v1.0.2 // indirect
)

// ffgo is vendored as a local fork: upstream loads libraries with
// purego.Dlopen, which does not exist on Windows, so the bindings could not
// even compile there. See third_party/VENDORED.md.
replace github.com/team-quaver/ffgo => ./third_party/ffgo

// purego-pipewire does not expose stream/app properties, so the OS mixer shows
// the Electron process ("electron") instead of Quaver Music. The local fork
// passes application.name/media.name/node.name through pw_stream_new_simple.
replace github.com/bnema/purego-pipewire => ./third_party/purego-pipewire
