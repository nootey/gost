import { defineStore } from "pinia";
import apiClient from "../api/api_client.ts";
import type { UploadResponse } from "../../domains/file/models.ts";

export const useFileStore = defineStore("file", {
    state: () => ({
        apiPrefix: "files",
    }),
    actions: {
        async upload(file: File): Promise<UploadResponse> {
            const body = new FormData();
            body.append("file", file);
            const response = await apiClient.post<UploadResponse>(this.apiPrefix, body);
            return response.data;
        },
    },
});
