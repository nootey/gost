import { defineStore } from "pinia";
import apiClient from "../api/api_client.ts";
import type { Page } from "../api/api_models.ts";
import type {
    FolderForm,
    FolderPath,
    FolderResponse,
    ListQuery,
    RenameForm,
    RenameResponse,
    StoredFile,
    UploadResponse,
} from "../../domains/file/models.ts";

// The API client leaves null params out.
export const useFileStore = defineStore("file", {
    state: () => ({
        apiPrefix: "files",
        folderPrefix: "folders",
    }),
    actions: {
        async list(query: ListQuery): Promise<Page<StoredFile>> {
            const response = await apiClient.get<Page<StoredFile>>(this.apiPrefix, { params: { ...query } });
            return response.data;
        },

        async upload(file: File, folderId: number | null): Promise<UploadResponse> {
            const body = new FormData();
            body.append("file", file);
            const response = await apiClient.post<UploadResponse>(this.apiPrefix, body, {
                params: { parent_id: folderId },
            });
            return response.data;
        },

        async createFolder(form: FolderForm): Promise<FolderResponse> {
            const response = await apiClient.post<FolderResponse>(this.folderPrefix, form);
            return response.data;
        },

        async rename(id: number, form: RenameForm): Promise<RenameResponse> {
            const response = await apiClient.patch<RenameResponse>(`${this.apiPrefix}/${id}`, form);
            return response.data;
        },

        async resolveFolder(dir: string): Promise<FolderPath> {
            const response = await apiClient.get<FolderPath>(`${this.folderPrefix}/resolve`, { params: { dir } });
            return response.data;
        },
    },
});
