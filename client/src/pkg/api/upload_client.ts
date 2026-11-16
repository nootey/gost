import { DetailedError, Upload } from "tus-js-client";
import apiClient, { endSession } from "./api_client.ts";
import { ApiError } from "./api_models.ts";
import type { PreviousUpload } from "tus-js-client";

// Each request carries one chunk, so it stays well under the request timeout of the API and the proxies.
// Without a chunk size, tus-js-client sends the whole file in one request.
const CHUNK_SIZE = 32 * 1024 * 1024;

// A short network drop goes on by itself. After a longer one the upload fails, and a new drop of the same file into
// the same folder goes on from where it stopped.
const RETRY_DELAYS = [0, 1000, 3000, 5000, 10000, 20000, 30000];

export interface ChunkedUpload<T> {
    // done gives the API answer, or null after cancel. It fails with an ApiError.
    done: Promise<T | null>;
    // cancel stops the upload and removes what the server has of it.
    cancel: () => Promise<void>;
}

// startUpload sends file to an upload path of the API in chunks, over the tus protocol (https://tus.io).
// onProgress gets the bytes sent so far.
export function startUpload<T>(
    path: string,
    file: File,
    metadata: Record<string, string>,
    onProgress: (sent: number, total: number) => void,
): ChunkedUpload<T> {
    let resolve!: (value: T | null) => void;
    let reject!: (reason: unknown) => void;
    const done = new Promise<T | null>((res, rej) => {
        resolve = res;
        reject = rej;
    });

    const upload = new Upload(file, {
        // tus-js-client reads the upload URL relative to the endpoint, so the endpoint must be a full URL.
        endpoint: new URL(apiClient.url(path), window.location.origin).href,
        chunkSize: CHUNK_SIZE,
        retryDelays: RETRY_DELAYS,
        metadata,
        removeFingerprintOnSuccess: true,
        onProgress,
        onSuccess: ({ lastResponse }) => {
            try {
                resolve(JSON.parse(lastResponse.getBody()) as T);
            } catch {
                reject(new ApiError("The server sent no answer for the upload.", lastResponse.getStatus(), null));
            }
        },
        onError: (error) => reject(toApiError(error)),
    });

    let cancelled = false;

    // A file that went up in part before goes on from there, if it goes to the same folder under the same name.
    upload
        .findPreviousUploads()
        .then((previous) => {
            const match = previous
                .filter((p) => sameMetadata(p, metadata))
                .sort((a, b) => b.creationTime.localeCompare(a.creationTime))[0];
            if (match) upload.resumeFromPreviousUpload(match);
        })
        .catch(() => undefined)
        .finally(() => {
            if (!cancelled) upload.start();
        });

    return {
        done,
        cancel: async () => {
            cancelled = true;
            // The server may have removed the upload already, and the upload is over either way.
            await upload.abort(true).catch(() => undefined);
            resolve(null);
        },
    };
}

function sameMetadata(previous: PreviousUpload, metadata: Record<string, string>): boolean {
    const keys = Object.keys(metadata);
    return (
        keys.length === Object.keys(previous.metadata).length &&
        keys.every((key) => previous.metadata[key] === metadata[key])
    );
}

// toApiError gives an upload error the shape of the other API errors, so the toasts read it the same way.
function toApiError(error: Error | DetailedError): Error {
    if (!(error instanceof DetailedError)) return error;

    const response = error.originalResponse;
    if (!response) return new ApiError("Network Error", 0, null, true);

    let data: unknown = null;
    try {
        data = JSON.parse(response.getBody());
    } catch {
        // tusd's own errors are plain text.
    }
    if (response.getStatus() === 401) void endSession();

    const message = (data as { message?: string } | null)?.message ?? error.message;
    return new ApiError(message, response.getStatus(), data);
}
