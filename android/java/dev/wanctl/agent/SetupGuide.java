package dev.wanctl.agent;

import android.animation.ValueAnimator;
import android.content.Context;
import android.graphics.Canvas;
import android.graphics.Paint;
import android.graphics.RectF;
import android.view.View;
import android.view.animation.DecelerateInterpolator;

/** An explanatory phone illustration, never a substitute for permission state. */
final class SetupGuide extends View {
    private final Paint paint = new Paint(Paint.ANTI_ALIAS_FLAG);
    private ValueAnimator animator;
    private float progress = 1;

    private final boolean notifications, battery;

    SetupGuide(Context context, boolean notifications, boolean battery) {
        super(context);
        this.notifications = notifications;
        this.battery = battery;
        setContentDescription("设置示意动画：开启运行通知和后台活动");
    }

    @Override
    protected void onAttachedToWindow() {
        super.onAttachedToWindow();
        start();
    }

    @Override
    protected void onDetachedFromWindow() {
        stop();
        super.onDetachedFromWindow();
    }

    @Override
    protected void onWindowVisibilityChanged(int visibility) {
        super.onWindowVisibilityChanged(visibility);
        if (visibility == VISIBLE) start();
        else stop();
    }

    private void start() {
        if (animator != null
                || (notifications && battery)
                || !isAttachedToWindow()
                || !ValueAnimator.areAnimatorsEnabled()) return;
        animator = ValueAnimator.ofFloat(0, 1);
        animator.setDuration(2800);
        animator.setRepeatCount(ValueAnimator.INFINITE);
        animator.setRepeatMode(ValueAnimator.RESTART);
        animator.setInterpolator(new DecelerateInterpolator());
        animator.addUpdateListener(
                a -> {
                    progress = (float) a.getAnimatedValue();
                    invalidate();
                });
        animator.start();
    }

    private void stop() {
        if (animator != null) {
            animator.cancel();
            animator = null;
        }
    }

    @Override
    protected void onDraw(Canvas canvas) {
        super.onDraw(canvas);
        float scale = getHeight() / 180f;
        canvas.save();
        canvas.translate(getWidth() / 2f - 96 * scale, 0);
        canvas.scale(scale, scale);
        paint.setColor(0xFF1D1D1F);
        paint.setStyle(Paint.Style.STROKE);
        paint.setStrokeWidth(2);
        canvas.drawRoundRect(new RectF(36, 2, 156, 178), 20, 20, paint);
        canvas.drawLine(82, 13, 110, 13, paint);
        canvas.drawLine(80, 167, 112, 167, paint);
        paint.setStyle(Paint.Style.FILL);
        paint.setColor(0xFFF0F0F3);
        canvas.drawRoundRect(new RectF(16, 44, 176, 99), 12, 12, paint);
        paint.setColor(0xFF1D1D1F);
        paint.setTextSize(12);
        canvas.drawText("wanctl", 30, 65, paint);
        paint.setColor(0xFF69696E);
        paint.setTextSize(10);
        canvas.drawText("运行通知", 30, 83, paint);
        paint.setColor(0xFF69696E);
        canvas.drawText("后台活动", 51, 127, paint);
        float on = battery ? 1 : Math.min(1, Math.max(0, (progress - .25f) * 3));
        paint.setColor(on > .5f ? 0xFF0066CC : 0xFFD5D5DA);
        canvas.drawRoundRect(new RectF(111, 113, 144, 133), 10, 10, paint);
        paint.setColor(0xFFFFFFFF);
        canvas.drawCircle(121 + 13 * on, 123, 7, paint);
        paint.setColor(0xFF0066CC);
        paint.setAlpha(notifications ? 255 : (int) (255 * Math.min(1, progress * 4)));
        canvas.drawCircle(157, 70, 5, paint);
        paint.setAlpha(255);
        canvas.restore();
    }
}
