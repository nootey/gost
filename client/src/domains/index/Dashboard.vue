<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { useFileStore } from "../../pkg/stores/file_store.ts";
import { useToastStore } from "../../pkg/stores/toast_store.ts";
import UploadQueue from "../file/components/UploadQueue.vue";
import FilesPaginated from "../file/components/FilesPaginated.vue";
import FolderBreadcrumb from "../file/components/FolderBreadcrumb.vue";
import NewItemDialog from "../file/components/NewItemDialog.vue";
import { joinDir } from "../../pkg/utils/folder_route.ts";
import type { MenuItem } from "primevue/menuitem";
import type { NewItemKind, PathEntry } from "../file/models.ts";

const route = useRoute();
const router = useRouter();
const fileStore = useFileStore();
const toastStore = useToastStore();

const filesRef = ref<{ refresh: () => Promise<void> } | null>(null);
const uploadRef = ref<InstanceType<typeof UploadQueue> | null>(null);
const newMenuRef = ref<{ toggle: (event: Event) => void } | null>(null);

const routeDir = computed<string>(() => (typeof route.query.dir === "string" ? route.query.dir : ""));

// The page switches to a folder only after its path loads, so an unknown folder never shows as an empty one.
const folderId = ref<number | null>(null);
const path = ref<PathEntry[]>([]);
const ready = ref<boolean>(false);
const newItemVisible = ref<boolean>(false);
const newItemKind = ref<NewItemKind>("folder");
// Moving over a child element fires a leave for the parent, so a count keeps the highlight steady. Only a drag of
// files counts, so dragging a link or text on the page does nothing.
const dragDepth = ref<number>(0);

const dropping = computed<boolean>(() => dragDepth.value > 0 && ready.value && !uploadRef.value?.busy);
const newMenuItems = computed<MenuItem[]>(() => [
    {
        label: "Upload files",
        icon: "pi pi-upload",
        disabled: uploadRef.value?.busy,
        command: () => uploadRef.value?.chooseFiles(),
    },
    {
        label: "Upload folder",
        icon: "pi pi-folder-open",
        disabled: uploadRef.value?.busy,
        command: () => uploadRef.value?.chooseFolder(),
    },
    { separator: true },
    { label: "New folder", icon: "pi pi-folder-plus", command: () => openNewItem("folder") },
    { label: "New file", icon: "pi pi-file-plus", command: () => openNewItem("file") },
]);
const dir = computed<string>(() => path.value.reduce((parent, entry) => joinDir(parent, entry.name), ""));

// Only the newest request may switch the folder, so a slow old one cannot undo a later click.
let latestRequest = 0;

async function openFolder(target: string): Promise<void> {
    const request = ++latestRequest;
    if (target === "") {
        folderId.value = null;
        path.value = [];
        ready.value = true;
        return;
    }

    try {
        const response = await fileStore.resolveFolder(target);
        if (request !== latestRequest) return;
        path.value = response.path;
        folderId.value = response.path.at(-1)?.id ?? null;
        ready.value = true;
    } catch (error) {
        if (request !== latestRequest) return;
        toastStore.errorResponseToast(error);
        await router.replace({ name: "dashboard" });
    }
}

async function onChanged(): Promise<void> {
    await filesRef.value?.refresh();
}

function toggleNewMenu(event: Event): void {
    newMenuRef.value?.toggle(event);
}

function openNewItem(kind: NewItemKind): void {
    newItemKind.value = kind;
    newItemVisible.value = true;
}

function hasFiles(event: DragEvent): boolean {
    return event.dataTransfer?.types.includes("Files") ?? false;
}

function onDragEnter(event: DragEvent): void {
    if (hasFiles(event)) dragDepth.value++;
}

function onDragLeave(event: DragEvent): void {
    if (hasFiles(event)) dragDepth.value = Math.max(0, dragDepth.value - 1);
}

// The drop handlers always cancel the browser's own action, so a drop never opens the file in place of the app.
function onDragOver(event: DragEvent): void {
    if (event.dataTransfer) event.dataTransfer.dropEffect = dropping.value ? "copy" : "none";
}

async function onDrop(event: DragEvent): Promise<void> {
    const accepted = dropping.value;
    dragDepth.value = 0;
    if (accepted) await uploadRef.value?.drop(event);
}

watch(routeDir, openFolder, { immediate: true });
</script>

<template>
    <div
        class="flex flex-col gap-8"
        @dragenter.prevent="onDragEnter"
        @dragover.prevent="onDragOver"
        @dragleave="onDragLeave"
        @drop.prevent="onDrop"
    >
        <div class="flex flex-col gap-1">
            <h1 class="text-xl font-semibold tracking-tight text-fg sm:text-2xl">Files</h1>
            <p class="text-sm text-fg-muted">
                Files are stored on this server's disk. Only you can see your files. Drop files or folders on this page
                to upload them.
            </p>
        </div>

        <div class="flex flex-col gap-3">
            <div class="flex items-center justify-between gap-3">
                <FolderBreadcrumb :path="path" />
                <div class="flex shrink-0 items-center gap-3">
                    <UploadQueue ref="uploadRef" :folder-id="folderId" :disabled="!ready" @changed="onChanged" />
                    <Button
                        label="New"
                        icon="pi pi-plus"
                        size="small"
                        aria-haspopup="true"
                        aria-controls="new-menu"
                        :disabled="!ready"
                        @click="toggleNewMenu"
                    />
                    <Menu id="new-menu" ref="newMenuRef" :model="newMenuItems" popup />
                </div>
            </div>

            <div class="rounded-2xl transition-shadow" :class="{ 'ring-2 ring-accent': dropping }">
                <FilesPaginated v-if="ready" ref="filesRef" :folder-id="folderId" :dir="dir" />
            </div>
        </div>

        <NewItemDialog
            v-model:visible="newItemVisible"
            :folder-id="folderId"
            :kind="newItemKind"
            @created="onChanged"
        />
    </div>
</template>
