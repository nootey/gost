import { defineStore } from "pinia";
import { ApiError } from "../api/api_models.ts";
import { useFileStore } from "./file_store.ts";
import { useToastStore } from "./toast_store.ts";
import type { Download } from "../../domains/file/models.ts";

// How often the client reads the list again while a zip builds.
const POLL_MS = 2000;

// One timer for the whole app, so the list never polls twice.
let timer: ReturnType<typeof setTimeout> | undefined;

// Only the newest read may write the list, so a slow old answer cannot replace a new one, and a read that ends after
// a logout writes nothing.
let latestRead = 0;

// The download store holds the user's zips, as the server lists them. While a zip builds, it reads the list again.
// A ready zip waits in the list until the user downloads it there, so the browser never starts a download by itself.
export const useDownloadStore = defineStore("download", {
    state: () => ({
        downloads: [] as Download[],
    }),
    getters: {
        // The zips that build, and the ready zips the user did not download yet. The header badge shows it.
        pendingCount: (s): number =>
            s.downloads.filter((d) => d.state === "building" || (d.state === "ready" && !d.downloaded)).length,
        isBuilding: (s): boolean => s.downloads.some((d) => d.state === "building"),
    },
    actions: {
        // load reads the list. A zip that went from building to failed shows an error toast with the server's error.
        // A network error keeps the old list and waits on. Any other error ends the wait.
        async load(): Promise<void> {
            this.stop();
            const read = ++latestRead;
            const toastStore = useToastStore();
            try {
                const response = await useFileStore().listDownloads();
                if (read !== latestRead) return;
                const before = new Map(this.downloads.map((d) => [d.id, d.state]));
                for (const download of response.downloads) {
                    if (download.state === "failed" && before.get(download.id) === "building") {
                        toastStore.createErrorToast(download.name, download.error ?? "");
                    }
                }
                this.downloads = response.downloads;
            } catch (error) {
                if (read !== latestRead) return;
                if (!(error instanceof ApiError && error.isNetworkError)) {
                    toastStore.errorResponseToast(error);
                    return;
                }
            }
            if (this.isBuilding) this.schedule();
        },

        // add puts a zip the user just asked for on top of the list and starts the wait.
        add(download: Download): void {
            this.downloads = [download, ...this.downloads.filter((d) => d.id !== download.id)];
            this.schedule();
        },

        // markDownloaded drops a ready zip from the badge at once. The server marks it too when it sends the zip.
        markDownloaded(id: number): void {
            const download = this.downloads.find((d) => d.id === id);
            if (download) download.downloaded = true;
        },

        async remove(id: number): Promise<void> {
            const toastStore = useToastStore();
            try {
                const response = await useFileStore().removeDownload(id);
                toastStore.successResponseToast(response);
            } catch (error) {
                toastStore.errorResponseToast(error);
            }
            await this.load();
        },

        schedule(): void {
            if (timer === undefined) timer = setTimeout(() => void this.load(), POLL_MS);
        },

        stop(): void {
            clearTimeout(timer);
            timer = undefined;
        },

        clear(): void {
            this.stop();
            latestRead++;
            this.downloads = [];
        },
    },
});
