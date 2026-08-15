/**
 * The protocol version this client speaks. Sent in every handshake message
 * (create-room, join-room, rejoin-room) so the server can reject connections
 * from clients whose major version differs from the server's.
 *
 * Stamped from the repo-root `VERSION` file by `scripts/release-client.sh`.
 * The server compares only the major component — see
 * `server/internal/version/version.go` `MajorFromString`.
 */
export const PROTOCOL_VERSION = '0.8.1';
