package release

// AllowedSigners holds the public key(s) allowed to sign releases, in
// ssh-keygen's allowed_signers format (one key per line):
//
//	tc-release namespaces="tc-release" ssh-ed25519 AAAA… maintainer@example
//
// install.sh embeds the same text (TestInstallSignersMatch keeps them in
// sync). While it is the placeholder, this build refuses every update
// (ErrNoSigningKey) instead of trusting an unsigned checksums.txt.
const AllowedSigners = "REPLACE_WITH_MAINTAINER_KEY"

// Placeholder is AllowedSigners before a maintainer key is embedded.
const Placeholder = "REPLACE_WITH_MAINTAINER_KEY"
