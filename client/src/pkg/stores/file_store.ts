import { defineStore } from "pinia";
import apiClient from "../api/api_client.ts";
import type { PaginatedFiles, UploadResponse } from "../../domains/file/models.ts";

export const useFileStore = defineStore("file", {
    state: () => ({
        apiPrefix: "files",
    }),
    actions: {
        async list(page: number, pageSize: number): Promise<PaginatedFiles> {
            const response = await apiClient.get<PaginatedFiles>(this.apiPrefix, {
                params: { page, page_size: pageSize },
            });
            return response.data;
        },

        async upload(file: File): Promise<UploadResponse> {
            const body = new FormData();
            body.append("file", file);
            const response = await apiClient.post<UploadResponse>(this.apiPrefix, body);
            return response.data;
        },
    },
});
