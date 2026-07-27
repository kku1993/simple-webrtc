# Room ID spec

This document describes the spec for room IDs in the game.
The IDs are specifically designed to:

1. support backend sharding
2. make it easy to type - including for non-English speakers
3. avoid randomly creating bad words

## How IDs are used

IDs are used to uniquely identify a room on a backend instance. Room IDs are globally unique.

## Format

IDs are formatted as:

```
[shard][nid]
```

All parts of the ID should be in lowercase to be more URL friendly. However, we
allow user to enter uppercase and for the fronted to display in uppercase for
aesthetics, while always converting to all lowercase in the background.

## Character set

In this document, whenever we mention base 32, we're referring to 
[Crockford's base 32](https://www.crockford.com/base32.html)
encoding and decoding rules.

Additionally, recall that we only use lowercase letters.

### shard

When starting a backend instance, the backend must be assigned to a shard. There
may be multiple backend servers in a shard, all should be configured with the
same shard name.

In the future, a loadbalancer can look at the first part of a room id and
identify the appropriate group of backends to handle it.

Shard name must be a single "alphabetic" character from base32, with digits
`[0-9]` reserved for future use.

Note for future: we should avoid using `0` (zero), `1` (one), and `2` (two) in
the future because they look similar to `O`, `L`, and `Z` - we want to keep
loadbalancer logic simple - avoid teaching it base32 fuzzy decoding logic.

### nid

This is a 5 digit number - the first digit is base32 while the rest are base 10.

- We only use 1 base32 number to avoid accidentially creating a bad word.
- The backend should randomly generate the nid on room creation.
- If there's a collision, backend should retry up to 5 times before returning an error.
- After a room is retired/expired, `nid` can be reused.

## Backend handling

When a backend receives a room id that doesn't match its shard name or is
otherwise malformed, it should return a 400 bad request with an error message.

Backend should automatically follow the fuzzy parsing logic set out in
[Crockford's base 32](https://www.crockford.com/base32.html)

## Frontend handling

Frontend should NOT perform any validation or fuzzy decoding - let the backend
handle it.

This allows flexibility to change the schema in the future without having to
force update all frontends.
