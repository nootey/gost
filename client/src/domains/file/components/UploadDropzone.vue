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

const props = defineProps<{ folderId: number | null; folderName?: string }>();
// changed fires when items were added, so the list can load again.
const emit = defineEmits<{ changed: [] }>();

const fileStore = useFileStore();
const toastStore = useToastStore();

// PrimeVue FileUpload reads only the plain file list of a drop, which has no folder contents, so the dropzone reads
// drops and its two inputs itself.
const fileInput = ref<HTMLInputElement | null>(null);
const folderInput = ref<HTMLInputElement | null>(null);
// Moving over a child element fires a leave for the parent, so a count keeps the highlight steady.
const dragDepth = ref<number>(0);
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

// The drop handlers always cancel the browser's own action, so a drop never opens the file in place of the app.
function onDragOver(event: DragEvent): void {
    if (event.dataTransfer) event.dataTransfer.dropEffect = busy.value ? "none" : "copy";
}

async function onDrop(event: DragEvent): Promise<void> {
    dragDepth.value = 0;
    if (busy.value || !event.dataTransfer) return;

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
</script>

<template>
    <div
        class="flex flex-col items-center justify-center gap-4 rounded-2xl border-2 border-dashed px-6 py-14 text-center transition-colors"
        :class="dragDepth > 0 && !busy ? 'border-accent bg-accent-soft' : 'border-edge bg-panel'"
        @dragenter.prevent="dragDepth++"
        @dragover.prevent="onDragOver"
        @dragleave="dragDepth = Math.max(0, dragDepth - 1)"
        @drop.prevent="onDrop"
    >
        <input ref="fileInput" type="file" multiple class="hidden" @change="onFilesChosen" />
        <input ref="folderInput" type="file" webkitdirectory class="hidden" @change="onFolderChosen" />

        <template v-if="preparing">
            <i class="pi pi-spin pi-spinner text-2xl text-accent" />
            <span class="text-sm text-fg-muted">Getting the upload ready…</span>
        </template>

        <template v-else-if="busy">
            <span class="grid size-14 place-items-center rounded-2xl bg-accent-soft text-accent">
                <i class="pi pi-cloud-upload text-2xl" />
            </span>
            <div class="flex w-full max-w-md flex-col gap-2">
                <div class="flex items-baseline justify-between gap-3">
                    <span class="truncate font-medium text-fg">{{ current?.name }}</span>
                    <span class="shrink-0 text-sm text-fg-muted">{{ percent }}%</span>
                </div>
                <ProgressBar :value="percent" :show-value="false" class="h-1.5" />
                <div class="flex items-baseline justify-between gap-3 text-xs text-fg-muted">
                    <span>
                        {{ vueHelper.formatBytes(doneBytes + sent) }} of {{ vueHelper.formatBytes(totalBytes) }}
                    </span>
                    <span v-if="queue.length > 1">
                        File {{ (index + 1).toLocaleString() }} of {{ queue.length.toLocaleString() }}
                    </span>
                </div>
            </div>
            <Button
                label="Cancel"
                icon="pi pi-times"
                size="small"
                severity="secondary"
                variant="outlined"
                :loading="cancelling"
                @click="cancel"
            />
        </template>

        <template v-else>
            <span class="grid size-14 place-items-center rounded-2xl bg-accent-soft text-accent">
                <i class="pi pi-cloud-upload text-2xl" />
            </span>
            <div class="flex flex-col gap-1">
                <span class="text-base font-medium text-fg">Drop files or folders here to upload</span>
                <span class="text-sm text-fg-muted">
                    <template v-if="folderName">
                        Files go into
                        <span class="font-medium text-fg">{{ folderName }}</span>
                    </template>
                    <template v-else>You can add many files or whole folders at a time.</template>
                </span>
            </div>
            <div class="flex flex-wrap justify-center gap-2">
                <Button label="Browse files" icon="pi pi-file" size="small" @click="fileInput?.click()" />
                <Button
                    label="Choose folder"
                    icon="pi pi-folder-open"
                    size="small"
                    severity="secondary"
                    variant="outlined"
                    @click="folderInput?.click()"
                />
            </div>
        </template>
    </div>
</template>
