import { defineStore } from "pinia";
import { ApiError } from "../api/api_models.ts";
import { startDownload } from "../utils/download_link.ts";
import { useAuthStore } from "./auth_store.ts";
import { useFileStore } from "./file_store.ts";
import { useToastStore } from "./toast_store.ts";

// How often the client asks for the state of the zips it waits for.
const POLL_MS = 2000;

// sessionStorage keeps the waiting zips when the user reloads, in this tab only. Logout clears it.
const STORAGE_KEY = "gost_downloads";

function isId(value: unknown): value is number {
    return Number.isInteger(value) && (value as number) > 0;
}

function readStoredIds(): number[] {
    try {
        const value: unknown = JSON.parse(sessionStorage.getItem(STORAGE_KEY) ?? "[]");
        return Array.isArray(value) ? value.filter(isId) : [];
    } catch {
        return [];
    }
}

// One timer for the whole app, so two waits never poll twice.
let timer: ReturnType<typeof setTimeout> | undefined;

// The download store waits for the zips the user asked for. When a zip is ready, the browser downloads it.
export const useDownloadStore = defineStore("download", {
    state: () => ({
        waiting: readStoredIds(),
    }),
    actions: {
        wait(id: number): void {
            if (!this.waiting.includes(id)) this.save([...this.waiting, id]);
            this.schedule();
        },

        // resume waits again for the zips from before a reload.
        resume(): void {
            if (this.waiting.length > 0) this.schedule();
        },

        schedule(): void {
            if (timer === undefined) timer = setTimeout(() => void this.check(), POLL_MS);
        },

        // check asks for each waiting zip once. A network error can pass, so that zip waits on. Any other error, such
        // as a 404 after the sweep removed the zip, ends the wait.
        async check(): Promise<void> {
            timer = undefined;
            if (!useAuthStore().isAuthenticated) {
                this.save([]);
                return;
            }

            const fileStore = useFileStore();
            const toastStore = useToastStore();
            for (const id of this.waiting) {
                try {
                    const download = await fileStore.getDownload(id);
                    if (download.state === "building") continue;
                    this.drop(id);
                    if (download.state === "ready") startDownload(fileStore.zipUrl(id));
                    else toastStore.createErrorToast(download.name, download.error ?? "");
                } catch (error) {
                    if (error instanceof ApiError && error.isNetworkError) continue;
                    this.drop(id);
                    toastStore.errorResponseToast(error);
                }
            }
            if (this.waiting.length > 0) this.schedule();
        },

        drop(id: number): void {
            this.save(this.waiting.filter((waiting) => waiting !== id));
        },

        save(ids: number[]): void {
            this.waiting = ids;
            if (ids.length > 0) sessionStorage.setItem(STORAGE_KEY, JSON.stringify(ids));
            else sessionStorage.removeItem(STORAGE_KEY);
        },
    },
});
