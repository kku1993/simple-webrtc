// Package roomid generates human-friendly, speakable room IDs of the form
//
//	[shard]-[adjective]-[noun]-[sequence]
//
// e.g. `us-golden-dragon-k3`. The format follows docs/DESIGN.md
// §"Identifier encoding / roomId" and is designed to be easy to read out loud
// and type, while still being collision-checked at generation time.
//
// The shard is a lowercase opaque tag assigned to the backend instance
// (see docs/DESIGN.md). For now it is always "us". The adjective and noun are
// drawn from curated, Chinese-culture-themed word lists containing only
// [a-z] characters. The sequence is a 2-digit base-36 (0-9, a-z) number that
// widens the ID space and absorbs collisions.
//
// The whole ID uses only [0-9a-z-], which is a subset of the protocol's
// allowed roomId character set [0-9a-z_-].
package roomid

import (
	"crypto/rand"
	"errors"
	"strings"
)

// Shard is the shard name baked into every generated room ID. It is lowercase
// per the room ID spec; the protocol's roomId character set is [0-9a-z_-],
// so an uppercase shard would be rejected by any future charset validation.
//
// For now every instance uses "us"; this is a constant rather than a config
// value deliberately — see docs/DESIGN.md §"Identifier encoding".
const Shard = "us"

// MaxRetries is the number of collision retries Generate performs before
// giving up, matching the room ID spec's "retry up to 5 times" guidance.
const MaxRetries = 5

// adjectives and nouns are copied from the mahjong-p2p `names` package
// (packages/names/src/index.ts): Chinese-culture-themed, no negative or
// vulgar connotations, [a-z] only.
//
// They are unexported slices (not arrays) so pickWord can index them with a
// runtime-random index without copying.

var adjectives = []string{
	"golden",
	"jade",
	"crimson",
	"azure",
	"scarlet",
	"silver",
	"pearl",
	"imperial",
	"celestial",
	"lucky",
	"hidden",
	"silent",
	"flying",
	"dancing",
	"roaring",
	"wandering",
	"drunken",
	"radiant",
	"ancient",
	"mighty",
	"gentle",
	"swift",
	"misty",
	"fiery",
	"humble",
	"smiling",
	"thundering",
	"moonlit",
	"blooming",
	"charming",
	"graceful",
	"glorious",
	"harmonious",
	"honored",
	"joyful",
	"lively",
	"noble",
	"peaceful",
	"prosperous",
	"resplendent",
	"serene",
	"splendid",
	"tranquil",
	"vibrant",
	"virtuous",
	"wise",
	"wondrous",
	"youthful",
	"abundant",
	"bold",
}

var nouns = []string{
	"dragon",
	"phoenix",
	"tiger",
	"crane",
	"sparrow",
	"koi",
	"panda",
	"monkey",
	"serpent",
	"turtle",
	"lotus",
	"peony",
	"plum",
	"willow",
	"bamboo",
	"lantern",
	"temple",
	"palace",
	"garden",
	"pavilion",
	"mountain",
	"river",
	"cloud",
	"moon",
	"wind",
	"emperor",
	"warrior",
	"monk",
	"blossom",
	"chrysanthemum",
	"orchid",
	"jasmine",
	"osmanthus",
	"camellia",
	"magnolia",
	"gardenia",
	"azalea",
	"peach",
	"lychee",
	"longan",
	"pomegranate",
	"gourd",
	"melon",
	"tea",
	"rice",
	"noodle",
	"dumpling",
	"mooncake",
	"silk",
	"porcelain",
	"bronze",
	"ink",
	"brush",
	"scroll",
	"fan",
	"umbrella",
	"kite",
	"candle",
	"firework",
	"drum",
	"gong",
	"flute",
	"pipa",
	"zither",
	"bell",
	"chime",
	"coin",
	"ingot",
	"talisman",
	"charm",
	"knot",
	"tassel",
	"seal",
	"mirror",
	"comb",
	"hairpin",
	"bracelet",
	"necklace",
	"robe",
	"qipao",
	"courtyard",
	"alley",
	"market",
	"teahouse",
	"pond",
	"lake",
	"stream",
	"waterfall",
	"spring",
	"sunrise",
	"sunset",
	"star",
	"comet",
	"ancestor",
	"sage",
	"scholar",
	"poet",
	"dancer",
	"fisherman",
	"merchant",
}

// base36 alphabet for the sequence suffix (lowercase).
const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"

// Generate returns a new room ID of the form shard-adjective-noun-sequence.
//
// exists is called for each candidate and must report whether the ID is
// already in use (e.g. live room or live tombstone). Generate retries with a
// fresh candidate on collision, up to MaxRetries times. The shard is forced
// to lowercase so the whole ID stays within [0-9a-z-].
func Generate(shard string, exists func(id string) bool) (string, error) {
	if exists == nil {
		return "", errors.New("roomid: exists callback is nil")
	}
	shard = strings.ToLower(shard)
	for i := 0; i < MaxRetries; i++ {
		id, err := candidate(shard)
		if err != nil {
			return "", err
		}
		if !exists(id) {
			return id, nil
		}
	}
	return "", errors.New("roomid: could not generate unique room id after retries")
}

// candidate builds a single shard-adjective-noun-sequence string. The
// sequence is two base-36 digits drawn from crypto/rand, giving 36*36 = 1296
// possibilities per (adjective, noun) pair.
func candidate(shard string) (string, error) {
	adj, err := pickWord(adjectives)
	if err != nil {
		return "", err
	}
	noun, err := pickWord(nouns)
	if err != nil {
		return "", err
	}
	seq, err := randomBase36(2)
	if err != nil {
		return "", err
	}
	return shard + "-" + adj + "-" + noun + "-" + seq, nil
}

// pickWord returns a cryptographically-uniform random element from list.
func pickWord(list []string) (string, error) {
	n := len(list)
	if n == 0 {
		return "", errors.New("roomid: empty word list")
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// 8 bytes -> uint64, mod n. For our list sizes (<= ~100) the modulo
	// bias is negligible (< 2^-32).
	idx := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 |
		uint64(b[3])<<32 | uint64(b[4])<<24 | uint64(b[5])<<16 |
		uint64(b[6])<<8 | uint64(b[7])
	return list[idx%uint64(n)], nil
}

// randomBase36 returns n lowercase base-36 digits drawn from crypto/rand.
func randomBase36(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = base36[int(b)%len(base36)]
	}
	return string(buf), nil
}
