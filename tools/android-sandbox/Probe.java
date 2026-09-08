package dev.wanctl.idsandbox;
import android.app.Activity;
import android.os.Bundle;
import android.util.Log;
import android.widget.TextView;
import java.io.*;
import java.nio.file.Files;
import java.util.*;
import java.util.concurrent.*;
public class Probe extends Activity {
 private static final String TAG="WANCTL_ID_SANDBOX";
 private File config;
 private String run(String... args)throws Exception {
  ProcessBuilder b=builder(args).redirectErrorStream(true);
  Process p=b.start();
  if(!p.waitFor(20,TimeUnit.SECONDS)){p.destroyForcibly();throw new Exception("command timeout");}
  ByteArrayOutputStream bytes=new ByteArrayOutputStream();byte[] buffer=new byte[4096];int n;while((n=p.getInputStream().read(buffer))!=-1)bytes.write(buffer,0,n);String out=bytes.toString("UTF-8");
  if(p.exitValue()!=0)throw new Exception(out.trim());
  return out;
 }
 private ProcessBuilder builder(String... args){
  ArrayList<String> argv=new ArrayList<>();argv.add(getApplicationInfo().nativeLibraryDir+"/libwanctl.so");Collections.addAll(argv,args);
  ProcessBuilder b=new ProcessBuilder(argv);b.environment().put("WANCTL_CONFIG_DIR",config.toString());return b;
 }
 private String id(String out){for(String l:out.split("\n"))if(l.startsWith("device ID:"))return l.substring(10).trim();throw new IllegalStateException(out);}
 @Override public void onCreate(Bundle state){super.onCreate(state);TextView text=new TextView(this);text.setText("Testing the native binary in the Android app sandbox…");setContentView(text);
  new Thread(()->{try{
   config=new File(getFilesDir(),"test-"+System.nanoTime());config.mkdirs();
   String domain=new String(Files.readAllBytes(new File("/proc/self/attr/current").toPath()),java.nio.charset.StandardCharsets.UTF_8).trim();
   if(android.os.Process.myUid()<10000 || !domain.contains(":untrusted_app"))throw new Exception("probe is not running in an untrusted app sandbox");
   Log.i(TAG,"context uid="+android.os.Process.myUid()+" selinux="+domain);
   String first=id(run("id"));
   // Start with an existing certificate but no published installation ID.
   if(!new File(config,"device_id").delete())throw new Exception("test cleanup failed");
   ExecutorService pool=Executors.newFixedThreadPool(16);Set<String> ids=new HashSet<>();
   try{List<Future<String>> jobs=new ArrayList<>();for(int i=0;i<16;i++)jobs.add(pool.submit(()->id(run("id"))));for(Future<String> job:jobs)ids.add(job.get(30,TimeUnit.SECONDS));}finally{pool.shutdownNow();}
   if(ids.size()!=1)throw new Exception("concurrent IDs differ: "+ids.size());
   String current=id(run("id"));if(!ids.contains(current))throw new Exception("ID changed on restart");
   Log.i(TAG,"PASS concurrent_processes=16 distinct_ids=1 restart_preserved=true");
   Process agent=builder("agent","--relay","ws://10.0.2.2:18740","--token","sandbox-token","--name","sandbox-phone","--transport","ws").redirectErrorStream(true).start();
   CountDownLatch online=new CountDownLatch(1);StringBuilder logs=new StringBuilder();
   Thread reader=new Thread(()->{try(BufferedReader r=new BufferedReader(new InputStreamReader(agent.getInputStream()))){String line;while((line=r.readLine())!=null){logs.append(line).append('\n');if(line.contains("online via "))online.countDown();}}catch(Exception ignored){}});reader.start();
   boolean connected=online.await(20,TimeUnit.SECONDS);agent.destroy();agent.waitFor(5,TimeUnit.SECONDS);reader.join(2000);
   if(!connected)throw new Exception("agent did not connect: "+logs);
   Log.i(TAG,"PASS app_sandbox_agent_online=true");
   runOnUiThread(()->text.setText("PASS: concurrent ID creation, persistence, real relay connection"));
  }catch(Exception e){Log.e(TAG,"FAIL "+e.getMessage());runOnUiThread(()->text.setText("FAIL: "+e.getMessage()));}},"sandbox-probe").start();
 }
}
