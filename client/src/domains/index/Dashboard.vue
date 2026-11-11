<script setup lang="ts">
import { ref } from "vue";
import UploadDropzone from "../file/components/UploadDropzone.vue";
import FilesPaginated from "../file/components/FilesPaginated.vue";

const filesRef = ref<{ refresh: () => Promise<void> } | null>(null);

async function onUploaded(): Promise<void> {
    await filesRef.value?.refresh();
}
</script>

<template>
    <div class="mx-auto flex w-full max-w-4xl flex-col gap-8 px-4 py-10">
        <div class="flex flex-col gap-1">
            <h1 class="text-2xl font-semibold tracking-tight text-fg">Files</h1>
            <p class="text-sm text-fg-muted">Files are stored on this server's disk. Only you can see your files.</p>
        </div>

        <UploadDropzone @uploaded="onUploaded" />

        <section class="flex flex-col gap-3">
            <h2 class="text-sm font-medium text-fg-muted">Your files</h2>
            <FilesPaginated ref="filesRef" />
        </section>
    </div>
</template>
