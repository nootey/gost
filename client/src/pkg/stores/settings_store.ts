import { defineStore } from "pinia";
import apiClient from "../api/api_client.ts";
import { useToastStore } from "./toast_store.ts";
import type { Settings } from "../../domains/user/models.ts";

// The server's defaults. They match the lists before settings existed, and stay in use if the settings fail to load.
const DEFAULT_SETTINGS: Settings = {
    sort_favorites_first: false,
    sort_folders_first: true,
    show_hidden: false,
    show_extensions: true,
};

export const useSettingsStore = defineStore("settings", {
    state: () => ({
        apiPrefix: "settings",
        settings: { ...DEFAULT_SETTINGS },
        loaded: false,
    }),
    actions: {
        // The router loads the settings once, before the first page that needs a sign-in. A failed load shows its
        // error and keeps the defaults, and the next page tries again.
        async init(): Promise<void> {
            try {
                const response = await apiClient.get<Settings>(this.apiPrefix);
                this.settings = response.data;
                this.loaded = true;
            } catch (error) {
                useToastStore().errorResponseToast(error);
            }
        },

        // The server needs every setting, so a change sends all four.
        async save(settings: Settings): Promise<void> {
            const response = await apiClient.put<Settings>(this.apiPrefix, settings);
            this.settings = response.data;
        },
    },
});
