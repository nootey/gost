<script setup lang="ts">
import { computed, ref } from "vue";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import { ApiError } from "../../../pkg/api/api_models.ts";
import { readDrop, treeFromPaths } from "../../../pkg/utils/folder_drop.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import type { ChunkedUpload } from "../../../pkg/api/upload_client.ts";
import type { DroppedTree } from "../../../pkg/utils/folder_drop.ts";
import type { UploadResponse } from "../models.ts";

interface QueuedFile {
    file: File;
    folderId: number | null;
}

// disabled is on while the open folder still loads, so nothing goes into the folder before it.
const props = defineProps<{ folderId: number | null; disabled?: boolean }>();
// changed fires when items were added, so the list can load again.
const emit = defineEmits<{ changed: [] }>();

const fileStore = useFileStore();
const toastStore = useToastStore();

// PrimeVue FileUpload reads only the plain file list of a drop, which has no folder contents, so this component reads
// drops and its two inputs itself. The page's New menu opens the inputs.
const fileInput = ref<HTMLInputElement | null>(null);
const folderInput = ref<HTMLInputElement | null>(null);
const preparing = ref<boolean>(false);
const queue = ref<QueuedFile[]>([]);
const index = ref<number>(0);
const doneBytes = ref<number>(0);
const sent = ref<number>(0);
const cancelling = ref<boolean>(false);
let active: ChunkedUpload<UploadResponse> | null = null;

const busy = computed<boolean>(() => preparing.value || queue.value.length > 0);
const current = computed<File | null>(() => queue.value[index.value]?.file ?? null);
const totalBytes = computed<number>(() => queue.value.reduce((sum, queued) => sum + queued.file.size, 0));
const percent = computed<number>(() =>
    totalBytes.value > 0 ? Math.floor(((doneBytes.value + sent.value) / totalBytes.value) * 100) : 0,
);

async function drop(event: DragEvent): Promise<void> {
    if (busy.value || props.disabled || !event.dataTransfer) return;

    preparing.value = true;
    let tree: DroppedTree;
    try {
        tree = await readDrop(event.dataTransfer.items);
    } catch (error) {
        preparing.value = false;
        toastStore.errorResponseToast(error);
        return;
    }
    await upload(tree);
}

function chooseFiles(): void {
    fileInput.value?.click();
}

function chooseFolder(): void {
    folderInput.value?.click();
}

function onFilesChosen(event: Event): Promise<void> {
    const input = event.target as HTMLInputElement;
    const files = [...(input.files ?? [])];
    input.value = "";
    return upload({ files: files.map((file) => ({ file, dir: "" })), dirs: [] });
}

function onFolderChosen(event: Event): Promise<void> {
    const input = event.target as HTMLInputElement;
    const files = [...(input.files ?? [])];
    input.value = "";
    return upload(treeFromPaths(files));
}

// upload makes the folders of the tree in one call, then sends the files into them.
async function upload(tree: DroppedTree): Promise<void> {
    // Keep the folder from the drop, so opening another folder mid-batch does not split the batch.
    const folderId = props.folderId;
    let folders: Record<string, number> = {};

    preparing.value = true;
    try {
        if (tree.dirs.length > 0) {
            const response = await fileStore.createFolderTree({ parent_id: folderId, paths: tree.dirs });
            folders = response.folders;
            toastStore.successResponseToast(response);
            emit("changed");
        }
    } catch (error) {
        toastStore.errorResponseToast(error);
        return;
    } finally {
        preparing.value = false;
    }

    await sendFiles(tree.files.map(({ file, dir }) => ({ file, folderId: dir === "" ? folderId : folders[dir] })));
}

// Files go up one at a time. The batch ends with one success toast, and one error toast for each different error.
// A network error or a lost session stops the batch, because the files after it would fail the same way.
async function sendFiles(files: QueuedFile[]): Promise<void> {
    if (files.length === 0) return;

    queue.value = files;
    doneBytes.value = 0;
    cancelling.value = false;
    const errors = new Map<string, unknown>();
    let last: UploadResponse | null = null;
    let uploaded = 0;
    let lastChange = 0;

    for (const [i, { file, folderId }] of files.entries()) {
        if (cancelling.value) break;
        index.value = i;
        sent.value = 0;
        active = fileStore.upload(file, folderId, (bytes) => {
            sent.value = bytes;
        });
        try {
            const response = await active.done;
            if (response) {
                uploaded++;
                last = response;
                // The list loads again at most once a second, so a folder of small files does not flood the API.
                if (Date.now() - lastChange > 1000) {
                    lastChange = Date.now();
                    emit("changed");
                }
            }
        } catch (error) {
            const message = error instanceof Error ? error.message : String(error);
            if (!errors.has(message)) errors.set(message, error);
            if (error instanceof ApiError && (error.isNetworkError || error.status === 401)) break;
        }
        doneBytes.value += file.size;
        sent.value = 0;
    }

    active = null;
    queue.value = [];
    emit("changed");
    errors.forEach((error) => toastStore.errorResponseToast(error));
    if (last) {
        toastStore.successResponseToast(
            uploaded === 1 ? last : { title: last.title, message: `${uploaded} files uploaded.` },
        );
    }
}

// Cancel stops the file that goes up now, removes its part on the server, and drops the rest of the batch.
async function cancel(): Promise<void> {
    cancelling.value = true;
    await active?.cancel();
}

defineExpose({ busy, drop, chooseFiles, chooseFolder });
</script>

<template>
    <input ref="fileInput" type="file" multiple class="hidden" @change="onFilesChosen" />
    <input ref="folderInput" type="file" webkitdirectory class="hidden" @change="onFolderChosen" />

    <div v-if="preparing" class="flex h-8 items-center gap-2 text-sm text-fg-muted">
        <i class="pi pi-spin pi-spinner text-accent" />
        <span>Getting the upload ready…</span>
    </div>

    <div v-else-if="busy" class="flex w-48 items-center gap-2 sm:w-72">
        <div class="flex min-w-0 flex-1 flex-col gap-1">
            <div class="flex items-baseline justify-between gap-3 text-xs">
                <span class="truncate font-medium text-fg" :title="current?.name">{{ current?.name }}</span>
                <span class="shrink-0 tabular-nums text-fg-muted">{{ percent }}%</span>
            </div>
            <ProgressBar :value="percent" :show-value="false" class="h-1.5" />
            <div class="flex items-baseline justify-between gap-3 text-xs tabular-nums text-fg-muted">
                <span class="truncate">
                    {{ vueHelper.formatBytes(doneBytes + sent) }} of {{ vueHelper.formatBytes(totalBytes) }}
                </span>
                <span v-if="queue.length > 1" class="shrink-0">
                    File {{ (index + 1).toLocaleString() }} of {{ queue.length.toLocaleString() }}
                </span>
            </div>
        </div>
        <Button
            icon="pi pi-times"
            size="small"
            severity="secondary"
            variant="text"
            rounded
            aria-label="Cancel upload"
            title="Cancel upload"
            :loading="cancelling"
            @click="cancel"
        />
    </div>
</template>
