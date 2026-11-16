import { defineStore } from "pinia";
import apiClient from "../api/api_client.ts";
import { startUpload } from "../api/upload_client.ts";
import type { Page, SuccessResponse } from "../api/api_models.ts";
import type { ChunkedUpload } from "../api/upload_client.ts";
import type {
    Activity,
    ActivityQuery,
    DownloadList,
    DownloadResponse,
    Favorite,
    FavoriteQuery,
    FolderForm,
    FolderPath,
    FolderResponse,
    FolderTreeForm,
    FolderTreeResponse,
    ItemsForm,
    ListQuery,
    PasteForm,
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
        uploadPrefix: "uploads/",
        trashPrefix: "trash",
        downloadPrefix: "downloads",
    }),
    actions: {
        async list(query: ListQuery): Promise<Page<StoredFile>> {
            const response = await apiClient.get<Page<StoredFile>>(this.apiPrefix, { params: { ...query } });
            return response.data;
        },

        // The file goes up in chunks, so a file of any size works, and a broken upload goes on from where it stopped.
        upload(
            file: File,
            folderId: number | null,
            onProgress: (sent: number, total: number) => void,
        ): ChunkedUpload<UploadResponse> {
            const metadata: Record<string, string> = { filename: file.name };
            if (folderId !== null) metadata.parent_id = String(folderId);
            return startUpload<UploadResponse>(this.uploadPrefix, file, metadata, onProgress);
        },

        async createFolder(form: FolderForm): Promise<FolderResponse> {
            const response = await apiClient.post<FolderResponse>(this.folderPrefix, form);
            return response.data;
        },

        async createFolderTree(form: FolderTreeForm): Promise<FolderTreeResponse> {
            const response = await apiClient.post<FolderTreeResponse>(`${this.folderPrefix}/tree`, form);
            return response.data;
        },

        async getItem(id: number): Promise<StoredFile> {
            const response = await apiClient.get<StoredFile>(`${this.apiPrefix}/${id}`);
            return response.data;
        },

        async listActivity(id: number, query: ActivityQuery): Promise<Page<Activity>> {
            const response = await apiClient.get<Page<Activity>>(`${this.apiPrefix}/${id}/activity`, {
                params: { ...query },
            });
            return response.data;
        },

        async rename(id: number, form: RenameForm): Promise<RenameResponse> {
            const response = await apiClient.patch<RenameResponse>(`${this.apiPrefix}/${id}`, form);
            return response.data;
        },

        async move(form: PasteForm): Promise<SuccessResponse> {
            const response = await apiClient.post<SuccessResponse>(`${this.apiPrefix}/move`, form);
            return response.data;
        },

        // The answer comes once the copies are in place. A job copies what is under them.
        async copy(form: PasteForm): Promise<SuccessResponse> {
            const response = await apiClient.post<SuccessResponse>(`${this.apiPrefix}/copy`, form);
            return response.data;
        },

        // A link, not a request: the browser streams the file to disk and shows its own progress.
        downloadUrl(id: number): string {
            return apiClient.url(`${this.apiPrefix}/${id}/download`);
        },

        // A small copy of a picture for the list. The server makes it on the first request and keeps it.
        thumbnailUrl(id: number): string {
            return apiClient.url(`${this.apiPrefix}/${id}/thumbnail`);
        },

        // The answer comes once the zip job is in place. The download store waits for the zip.
        async requestZip(form: ItemsForm): Promise<DownloadResponse> {
            const response = await apiClient.post<DownloadResponse>(this.downloadPrefix, form);
            return response.data;
        },

        async listDownloads(): Promise<DownloadList> {
            const response = await apiClient.get<DownloadList>(this.downloadPrefix);
            return response.data;
        },

        async removeDownload(id: number): Promise<SuccessResponse> {
            const response = await apiClient.delete<SuccessResponse>(`${this.downloadPrefix}/${id}`);
            return response.data;
        },

        zipUrl(id: number): string {
            return apiClient.url(`${this.downloadPrefix}/${id}/file`);
        },

        async hide(form: ItemsForm): Promise<SuccessResponse> {
            const response = await apiClient.post<SuccessResponse>(`${this.apiPrefix}/hide`, form);
            return response.data;
        },

        async unhide(form: ItemsForm): Promise<SuccessResponse> {
            const response = await apiClient.post<SuccessResponse>(`${this.apiPrefix}/unhide`, form);
            return response.data;
        },

        async listFavorites(query: FavoriteQuery): Promise<Page<Favorite>> {
            const response = await apiClient.get<Page<Favorite>>("favorites", { params: { ...query } });
            return response.data;
        },

        async addFavorites(form: ItemsForm): Promise<SuccessResponse> {
            const response = await apiClient.post<SuccessResponse>("favorites", form);
            return response.data;
        },

        async removeFavorites(form: ItemsForm): Promise<SuccessResponse> {
            const response = await apiClient.post<SuccessResponse>("favorites/remove", form);
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
