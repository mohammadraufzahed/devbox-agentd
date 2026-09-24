/**
 * JSON-RPC 2.0 client for devbox-agentd over a Unix socket.
 *
 * Newline-delimited framing: each message is one JSON object per line.
 * Supports requests, server notifications (exec.output, pty.output,
 * service.logs) and `$/cancel` for aborting in-flight requests.
 *
 * The daemon is a singleton per user — `ensureAndConnect()` runs
 * `devbox-agentd ensure` (connect-or-start) before dialing.
 */

import { execFile } from "node:child_process";
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { createConnection, type Socket } from "node:net";
import { createInterface } from "node:readline";

export interface RpcErrorShape {
	code: number;
	message: string;
}

export class RpcError extends Error {
	readonly code: number;
	constructor(e: RpcErrorShape) {
		super(e.message);
		this.code = e.code;
	}
}

interface Pending {
	resolve: (v: unknown) => void;
	reject: (e: Error) => void;
	onNotification?: (params: Record<string, unknown>) => void;
}

export interface CallOptions {
	/** Forwarded to the daemon as `$/cancel` when aborted. */
	signal?: AbortSignal;
	/** Receives notifications whose params.reqId matches this request id. */
	onNotification?: (params: Record<string, unknown>) => void;
}

export function socketPath(): string {
	const dir = process.env.XDG_RUNTIME_DIR
		? join(process.env.XDG_RUNTIME_DIR, "devbox-agentd")
		: join(homedir(), ".local", "state", "devbox-agentd");
	return join(dir, "agent.sock");
}

export class DevboxClient {
	private sock: Socket | undefined;
	private nextId = 1;
	private pending = new Map<number, Pending>();
	private connecting: Promise<void> | undefined;
	private closed = false;

	/** Path of the socket we are (or will be) connected to. */
	readonly path = socketPath();

	/**
	 * Run `devbox-agentd ensure` and connect. Idempotent; concurrent callers
	 * share one attempt. Retries with backoff across calls on failure.
	 */
	ensureAndConnect(): Promise<void> {
		if (this.sock && !this.sock.destroyed) return Promise.resolve();
		if (this.connecting) return this.connecting;
		this.connecting = this.connectWithRetry()
			.catch((e) => {
				this.connecting = undefined;
				throw e;
			})
			.then(() => {
				this.connecting = undefined;
			});
		return this.connecting;
	}

	private async connectWithRetry(): Promise<void> {
		let lastErr: unknown;
		for (let attempt = 0; attempt < 4; attempt++) {
			try {
				await this.ensureDaemon();
				await this.dial();
				return;
			} catch (e) {
				lastErr = e;
				await sleep(200 * 2 ** attempt);
			}
		}
		throw lastErr instanceof Error ? lastErr : new Error(String(lastErr));
	}

	/** `devbox-agentd ensure` — the only process the extension ever spawns. */
	private ensureDaemon(): Promise<void> {
		return new Promise((resolve, reject) => {
			execFile(
				"devbox-agentd",
				["ensure"],
				{ timeout: 20_000 },
				(err, stdout) => {
					if (err) {
						reject(
							new Error(
								`devbox-agentd ensure failed: ${err.message}. Is devbox-agentd installed?`,
							),
						);
						return;
					}
					resolve();
				},
			);
		});
	}

	private dial(): Promise<void> {
		return new Promise((resolve, reject) => {
			const sock = createConnection(this.path);
			const onErr = (e: Error) => reject(e);
			sock.once("error", onErr);
			sock.once("connect", () => {
				sock.off("error", onErr);
				this.attach(sock);
				resolve();
			});
		});
	}

	private attach(sock: Socket): void {
		this.sock = sock;
		const rl = createInterface({ input: sock });
		rl.on("line", (line) => {
			if (!line.trim()) return;
			let msg: Record<string, unknown>;
			try {
				msg = JSON.parse(line) as Record<string, unknown>;
			} catch {
				return;
			}
			this.onMessage(msg);
		});
		sock.on("close", () => this.onClose());
		sock.on("error", () => this.onClose());
	}

	private onClose(): void {
		this.sock = undefined;
		for (const [, p] of this.pending) {
			p.reject(new Error("devbox-agentd connection closed"));
		}
		this.pending.clear();
	}

	private onMessage(msg: Record<string, unknown>): void {
		if (typeof msg.method === "string") {
			// Notification: route to pending request with matching reqId.
			const params = (msg.params ?? {}) as Record<string, unknown>;
			for (const [id, p] of this.pending) {
				if (p.onNotification && params.reqId === id) {
					p.onNotification(params);
				}
			}
			return;
		}
		const id = msg.id as number | undefined;
		if (id === undefined) return;
		const p = this.pending.get(id);
		if (!p) return;
		this.pending.delete(id);
		if (msg.error) {
			p.reject(new RpcError(msg.error as RpcErrorShape));
		} else {
			p.resolve(msg.result);
		}
	}

	/**
	 * Call a method. Connects (and ensures the daemon) first if needed, so a
	 * dead daemon is transparently restarted on the next call.
	 */
	async call<T = unknown>(
		method: string,
		params?: Record<string, unknown>,
		opts?: CallOptions,
	): Promise<T> {
		if (this.closed) throw new Error("devbox client closed");
		await this.ensureAndConnect();
		const id = this.nextId++;
		const req = JSON.stringify({
			jsonrpc: "2.0",
			id,
			method,
			params: params ?? {},
		});
		const promise = new Promise<T>((resolve, reject) => {
			this.pending.set(id, {
				resolve: resolve as (v: unknown) => void,
				reject,
				onNotification: opts?.onNotification,
			});
			this.sock!.write(req + "\n", (err) => {
				if (err) {
					this.pending.delete(id);
					reject(err);
				}
			});
		});
		if (opts?.signal) {
			const sig = opts.signal;
			const onAbort = () => this.cancel(id);
			if (sig.aborted) onAbort();
			else sig.addEventListener("abort", onAbort, { once: true });
			void promise.finally(() => sig.removeEventListener("abort", onAbort));
		}
		return promise;
	}

	/** Send `$/cancel` for an in-flight request id. */
	cancel(id: number): void {
		if (!this.sock || this.sock.destroyed) return;
		this.sock.write(
			JSON.stringify({ jsonrpc: "2.0", method: "$/cancel", params: { id } }) + "\n",
		);
	}

	/** Close the connection. Idempotent — safe to call from session_shutdown. */
	close(): void {
		this.closed = true;
		this.connecting = undefined;
		if (this.sock) {
			this.sock.destroy();
			this.sock = undefined;
		}
		this.onClose();
	}

	get connected(): boolean {
		return !!this.sock && !this.sock.destroyed;
	}
}

function sleep(ms: number): Promise<void> {
	return new Promise((r) => setTimeout(r, ms));
}

export function hasDaemonSocket(): boolean {
	return existsSync(socketPath());
}
