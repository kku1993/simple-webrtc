// Test doubles for the WebRTC surface the engine touches.
//
// These implement enough of `RTCPeerConnection` / `RTCDataChannel` to drive the
// engine deterministically in Node, with no browser and no `wrtc`. The fake
// peer connection maintains a real `signalingState` machine, so negotiation
// ordering and queueing are exercised rather than stubbed.

type Handler<E> = ((ev: E) => void) | null;

let channelIdCounter = 0;

/**
 * A fake `RTCDataChannel`. Two of them can be joined with {@link linkChannels}
 * so a `send` on one surfaces as a `message` on the other.
 */
export class FakeDataChannel {
  readonly label: string;
  readonly ordered: boolean;
  readonly maxRetransmits: number | null;
  readonly maxPacketLifeTime: number | null;
  readonly protocol: string;
  readonly id: number;
  readyState: RTCDataChannelState = 'connecting';
  bufferedAmount = 0;
  bufferedAmountLowThreshold = 0;
  binaryType = 'blob';

  /** Everything passed to `send()`, in order. */
  readonly sent: unknown[] = [];
  /** Set to make `send()` throw. */
  sendThrows: Error | null = null;

  onopen: Handler<Event> = null;
  onclose: Handler<Event> = null;
  onerror: Handler<Event> = null;
  onmessage: Handler<MessageEvent> = null;
  onbufferedamountlow: Handler<Event> = null;

  /** The channel joined via {@link linkChannels}, if any. */
  peer: FakeDataChannel | null = null;

  constructor(label: string, init: RTCDataChannelInit = {}) {
    this.label = label;
    this.ordered = init.ordered ?? true;
    this.maxRetransmits = init.maxRetransmits ?? null;
    this.maxPacketLifeTime = init.maxPacketLifeTime ?? null;
    this.protocol = init.protocol ?? '';
    this.id = channelIdCounter++;
  }

  send(data: unknown): void {
    if (this.sendThrows) throw this.sendThrows;
    if (this.readyState !== 'open') {
      throw new Error(`InvalidStateError: channel ${this.label} is ${this.readyState}`);
    }
    this.sent.push(data);
    this.peer?.deliver(data);
  }

  close(): void {
    if (this.readyState === 'closed') return;
    this.readyState = 'closed';
    this.onclose?.(new Event('close'));
  }

  // --- test drivers --------------------------------------------------------

  /** Transition to open and fire `onopen`. */
  open(): void {
    if (this.readyState === 'open') return;
    this.readyState = 'open';
    this.onopen?.(new Event('open'));
  }

  /** Deliver an inbound message. */
  deliver(data: unknown): void {
    this.onmessage?.({ data } as MessageEvent);
  }

  /** Fire `onerror` with the given error. */
  fireError(err: Error): void {
    this.onerror?.({ error: err } as unknown as Event);
  }

  /** Fire `onbufferedamountlow`. */
  fireDrain(): void {
    this.onbufferedamountlow?.(new Event('bufferedamountlow'));
  }

  asChannel(): RTCDataChannel {
    return this as unknown as RTCDataChannel;
  }
}

/** Join two channels so a `send` on one is delivered to the other. */
export function linkChannels(a: FakeDataChannel, b: FakeDataChannel): void {
  a.peer = b;
  b.peer = a;
}

export interface FakeSender {
  track: MediaStreamTrack | null;
  replaceTrack(t: MediaStreamTrack | null): Promise<void>;
}

/**
 * A fake `RTCPeerConnection`.
 *
 * Instances register themselves in the static {@link instances} array so tests
 * can retrieve the one an `RtcPeer` built for itself.
 */
export class FakePeerConnection {
  static instances: FakePeerConnection[] = [];
  static reset(): void {
    FakePeerConnection.instances = [];
    channelIdCounter = 0;
  }

  connectionState: RTCPeerConnectionState = 'new';
  iceConnectionState: RTCIceConnectionState = 'new';
  iceGatheringState: RTCIceGatheringState = 'new';
  signalingState: RTCSignalingState = 'stable';
  localDescription: RTCSessionDescriptionInit | null = null;
  remoteDescription: RTCSessionDescriptionInit | null = null;

  readonly config: RTCConfiguration | undefined;
  readonly createdChannels: FakeDataChannel[] = [];
  readonly addedCandidates: RTCIceCandidateInit[] = [];
  readonly addedTracks: { track: MediaStreamTrack; stream: MediaStream }[] = [];
  readonly removedSenders: FakeSender[] = [];
  readonly transceivers: { kind: string; init?: RTCRtpTransceiverInit }[] = [];
  /** Set to make `addIceCandidate` reject. */
  addIceCandidateRejects: Error | null = null;
  /** Set to make `removeTrack` throw (simulating removal during negotiation). */
  removeTrackThrows: Error | null = null;
  /** Set to make `createOffer` reject. */
  createOfferRejects: Error | null = null;
  closed = false;

  private offerCount = 0;
  private answerCount = 0;

  onicecandidate: Handler<RTCPeerConnectionIceEvent> = null;
  oniceconnectionstatechange: Handler<Event> = null;
  onconnectionstatechange: Handler<Event> = null;
  onsignalingstatechange: Handler<Event> = null;
  ondatachannel: Handler<RTCDataChannelEvent> = null;
  ontrack: Handler<RTCTrackEvent> = null;

  constructor(config?: RTCConfiguration) {
    this.config = config;
    FakePeerConnection.instances.push(this);
  }

  createDataChannel(label: string, init?: RTCDataChannelInit): RTCDataChannel {
    const ch = new FakeDataChannel(label, init);
    this.createdChannels.push(ch);
    return ch.asChannel();
  }

  createOffer(): Promise<RTCSessionDescriptionInit> {
    if (this.createOfferRejects) return Promise.reject(this.createOfferRejects);
    return Promise.resolve({ type: 'offer', sdp: `offer-sdp-${++this.offerCount}` });
  }

  createAnswer(): Promise<RTCSessionDescriptionInit> {
    return Promise.resolve({ type: 'answer', sdp: `answer-sdp-${++this.answerCount}` });
  }

  setLocalDescription(desc: RTCSessionDescriptionInit): Promise<void> {
    this.localDescription = desc;
    this.setSignalingState(desc.type === 'offer' ? 'have-local-offer' : 'stable');
    return Promise.resolve();
  }

  setRemoteDescription(desc: RTCSessionDescriptionInit): Promise<void> {
    this.remoteDescription = desc;
    this.setSignalingState(desc.type === 'offer' ? 'have-remote-offer' : 'stable');
    return Promise.resolve();
  }

  addIceCandidate(candidate: RTCIceCandidateInit): Promise<void> {
    if (this.addIceCandidateRejects) return Promise.reject(this.addIceCandidateRejects);
    this.addedCandidates.push(candidate);
    return Promise.resolve();
  }

  addTrack(track: MediaStreamTrack, stream: MediaStream): FakeSender {
    this.addedTracks.push({ track, stream });
    const sender: FakeSender = {
      track,
      replaceTrack(t) {
        sender.track = t;
        return Promise.resolve();
      },
    };
    return sender;
  }

  removeTrack(sender: FakeSender): void {
    if (this.removeTrackThrows) throw this.removeTrackThrows;
    this.removedSenders.push(sender);
  }

  addTransceiver(kind: string, init?: RTCRtpTransceiverInit): void {
    this.transceivers.push(init ? { kind, init } : { kind });
  }

  getStats(): Promise<RTCStatsReport> {
    return Promise.resolve(new Map() as unknown as RTCStatsReport);
  }

  close(): void {
    this.closed = true;
  }

  // --- test drivers --------------------------------------------------------

  setSignalingState(state: RTCSignalingState): void {
    this.signalingState = state;
    this.onsignalingstatechange?.(new Event('signalingstatechange'));
  }

  setConnectionState(state: RTCPeerConnectionState): void {
    this.connectionState = state;
    this.onconnectionstatechange?.(new Event('connectionstatechange'));
  }

  setIceConnectionState(state: RTCIceConnectionState): void {
    this.iceConnectionState = state;
    this.oniceconnectionstatechange?.(new Event('iceconnectionstatechange'));
  }

  emitIceCandidate(candidate: Partial<RTCIceCandidate> | null): void {
    this.onicecandidate?.({ candidate } as RTCPeerConnectionIceEvent);
  }

  /** Deliver an inbound data channel, as `ondatachannel` would. */
  emitDataChannel(channel: FakeDataChannel): void {
    this.ondatachannel?.({ channel: channel.asChannel() } as RTCDataChannelEvent);
  }

  emitTrack(track: MediaStreamTrack, streams: MediaStream[]): void {
    this.ontrack?.({ track, streams } as unknown as RTCTrackEvent);
  }

  /** The channel this connection created with the given label. */
  channel(label: string): FakeDataChannel {
    const ch = this.createdChannels.find((c) => c.label === label);
    if (!ch) throw new Error(`no channel created with label ${label}`);
    return ch;
  }
}

/** An `rtcImpl` override backed by {@link FakePeerConnection}. */
export const fakeRtcImpl = {
  RTCPeerConnection: FakePeerConnection as unknown as typeof RTCPeerConnection,
};

/** Await pending microtasks (the engine batches negotiation on one). */
export function tick(): Promise<void> {
  return new Promise((r) => setTimeout(r, 0));
}
