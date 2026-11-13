import { defineStore } from "pinia";
import apiClient from "../api/api_client.ts";
import type { Page, SuccessResponse } from "../api/api_models.ts";
import type {
    FolderForm,
    FolderPath,
    FolderResponse,
    ItemsForm,
    ListQuery,
    RenameForm,
    RenameResponse,
    StoredFile,
    TrashedFile,
    TrashQuery,
    UploadResponse,
} from "../../domains/file/models.ts";

// The API client leaves null params out.
export const useFileStore = defineStore("file", {
    state: () => ({
        apiPrefix: "files",
        folderPrefix: "folders",
        trashPrefix: "trash",
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

        async listTrash(query: TrashQuery): Promise<Page<TrashedFile>> {
            const response = await apiClient.get<Page<TrashedFile>>(this.trashPrefix, { params: { ...query } });
            return response.data;
        },

        async trash(form: ItemsForm): Promise<SuccessResponse> {
            const response = await apiClient.post<SuccessResponse>(this.trashPrefix, form);
            return response.data;
        },

        async restore(form: ItemsForm): Promise<SuccessResponse> {
            const response = await apiClient.post<SuccessResponse>(`${this.trashPrefix}/restore`, form);
            return response.data;
        },

        async purge(form: ItemsForm): Promise<SuccessResponse> {
            const response = await apiClient.post<SuccessResponse>(`${this.trashPrefix}/purge`, form);
            return response.data;
        },

        async resolveFolder(dir: string): Promise<FolderPath> {
            const response = await apiClient.get<FolderPath>(`${this.folderPrefix}/resolve`, { params: { dir } });
            return response.data;
        },
    },
});
