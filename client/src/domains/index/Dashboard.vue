<script setup lang="ts">
import { ref } from "vue";
import UploadDropzone from "../file/components/UploadDropzone.vue";
import vueHelper from "../../pkg/utils/vue_helper.ts";
import type { StoredFile } from "../file/models.ts";

const uploaded = ref<StoredFile[]>([]);

function onUploaded(file: StoredFile): void {
    uploaded.value.unshift(file);
}
</script>

<template>
    <div class="mx-auto flex w-full max-w-3xl flex-col gap-8 px-4 py-10">
        <div class="flex flex-col gap-1">
            <h1 class="text-2xl font-semibold tracking-tight text-fg">Upload files</h1>
            <p class="text-sm text-fg-muted">Files are stored on this server's disk.</p>
        </div>

        <UploadDropzone @uploaded="onUploaded" />

        <section v-if="uploaded.length" class="flex flex-col gap-3">
            <h2 class="text-sm font-medium text-fg-muted">Uploaded this session</h2>
            <ul class="flex flex-col divide-y divide-edge overflow-hidden rounded-2xl border border-edge bg-panel">
                <li v-for="file in uploaded" :key="file.id" class="flex items-center gap-3 px-4 py-3">
                    <span class="grid size-9 shrink-0 place-items-center rounded-lg bg-inset text-fg-muted">
                        <i :class="vueHelper.fileIcon(file.content_type)" />
                    </span>
                    <div class="flex min-w-0 flex-1 flex-col">
                        <span class="truncate text-sm font-medium text-fg">{{ file.name }}</span>
                        <span class="truncate text-xs text-fg-subtle">{{ file.content_type }}</span>
                    </div>
                    <span class="shrink-0 text-sm tabular-nums text-fg-muted">
                        {{ vueHelper.formatBytes(file.size_bytes) }}
                    </span>
                </li>
            </ul>
        </section>
    </div>
</template>
