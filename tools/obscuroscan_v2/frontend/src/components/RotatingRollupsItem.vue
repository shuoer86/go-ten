<template>
  <el-card class="centered-container" shadow="never" style="border-radius: 20px; ">
  <div class="slider-container" style="width: 100%;">
    <div class="slider-content" :class="{ 'slide-out': isAnimating }">
      <el-card v-for="card in displayedCards"  class="card-content box">
        <h3 class="header-text">Rollup: <ShortenedHash :hash="card.hash" /></h3>
        <p>&nbsp;</p>
        <h5>Last Batch: {{ card.LastBatchSeqNo }}</h5>
        <h5><ShortenedHash :hash="card.CompressionL1Head" /></h5>

      </el-card>
    </div>
  </div>
  </el-card>
</template>

<script>
import {computed, onMounted, onUnmounted} from "vue";
import {useRollupStore} from "@/stores/rollupStore";
import ShortenedHash from "@/components/helper/ShortenedHash.vue";

export default {
  name: "RotatingRollupsItem",
  components: {ShortenedHash},

  setup() {
    const rollupsStore = useRollupStore()

    // Reload rollup data onMount
    onMounted(() => {
      rollupsStore.fetch()
    })

    return {
      displayedCards:  computed(() => rollupsStore.rollups.get()),
      isAnimating: false
    }
  },
}
</script>

<style scoped>

.header-text {
  color: #5973B8;
  font-weight: bold;
}

.box {
  border-radius: 15px;
  background: #F4F6FF;
}

.centered-container {
  display: flex;
  justify-content: center;  /* Center children horizontally */
  align-items: center;      /* Center children vertically */
}

.slider-container {
  overflow-x: auto;
  width: 1000px;  /* Adjust based on your design */
  white-space: nowrap;
}

.slider-content {
  display: flex;
}

.card-content {
  display: inline-block;
  width: 200px;  /* If 5 cards need to be displayed at a time, and container width is 1000px, then each card can be approximately 200px wide */
  margin-right: 20px;  /* Adjust as needed */
  text-align: center;
}


.slide-out {
  animation: slideOut 1s forwards; /* Animation to slide the cards to the left */
}

@keyframes slideOut {
  0% { transform: translateX(0%); }
  100% { transform: translateX(-20%); } /* Assuming each card takes 20% width */
}

</style>